package ssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
	"go.kenn.io/kwt/service"
)

type fakePersistentManager struct {
	mu              sync.Mutex
	generation      openssh.Generation
	connectErr      error
	argumentErr     error
	connectCalls    int
	argumentCalls   int
	touchCalls      int
	disconnectCalls int
	disconnectErr   error
	disconnectStart chan struct{}
	disconnectWait  <-chan struct{}
	disconnectOnce  sync.Once
}

func (m *fakePersistentManager) ConnectWithRunner(
	context.Context,
	string,
	openssh.Target,
	openssh.RunSSH,
) (openssh.Generation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connectCalls++
	return m.generation, m.connectErr
}

func (m *fakePersistentManager) ConnectionArguments(
	string,
	openssh.Generation,
) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.argumentCalls++
	return []string{"-S", "control"}, m.argumentErr
}

func (m *fakePersistentManager) TouchActivity(string, openssh.Generation) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touchCalls++
	return true
}

func (m *fakePersistentManager) Disconnect(context.Context, string) error {
	m.mu.Lock()
	m.disconnectCalls++
	disconnectErr := m.disconnectErr
	disconnectStart := m.disconnectStart
	disconnectWait := m.disconnectWait
	m.mu.Unlock()
	if disconnectStart != nil {
		m.disconnectOnce.Do(func() { close(disconnectStart) })
	}
	if disconnectWait != nil {
		<-disconnectWait
	}
	return disconnectErr
}

func (m *fakePersistentManager) counts() (connect, arguments, touch, disconnect int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectCalls, m.argumentCalls, m.touchCalls, m.disconnectCalls
}

func directSnapshot(identity string) RouteSnapshot {
	return RouteSnapshot{
		LogicalTarget:    Target{Hostname: "build.example.test"},
		RouteIdentity:    identity,
		ProjectionPolicy: ProjectionPolicyV1,
		Targets: []ResolvedTarget{{
			LogicalTarget:   Target{Hostname: "build.example.test"},
			EffectiveTarget: Target{User: "deploy", Hostname: "build.internal", Port: 22},
			Projection: ExecutionProjection{Arguments: []string{
				"-F", os.DevNull, "-o", "HostName=build.internal",
			}},
		}},
	}
}

func TestManagerSharesGenerationUntilFinalLeaseRelease(t *testing.T) {
	persistent := &fakePersistentManager{generation: 11}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("route-one")
	resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }

	first, err := manager.Acquire(context.Background(), LeaseRequest{Snapshot: snapshot}, resolve)
	require.NoError(t, err)
	assert.Equal(t, LeaseModeMultiplexed, first.Mode())
	second, err := manager.Acquire(context.Background(), LeaseRequest{Snapshot: snapshot}, resolve)
	require.NoError(t, err)
	assert.Equal(t, uint64(11), first.Generation())
	assert.Equal(t, first.Generation(), second.Generation())

	arguments, err := first.Arguments(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"-S", "control"}, arguments)
	require.NoError(t, first.Touch())
	require.NoError(t, first.Release())
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 0, disconnects)
	require.NoError(t, second.Release())
	connects, argumentCalls, touches, disconnects := persistent.counts()
	assert.Equal(t, 2, connects)
	assert.Equal(t, 1, argumentCalls)
	assert.Equal(t, 1, touches)
	assert.Equal(t, 1, disconnects)
}

func TestManagerKeepsFinalLeaseWarmUntilIdleTimeout(t *testing.T) {
	persistent := &fakePersistentManager{generation: 14}
	manager := NewManager(ManagerOptions{
		Persistent:  persistent,
		IdleTimeout: 20 * time.Millisecond,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("route-one")
	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)

	require.NoError(t, lease.Release())
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 0, disconnects)
	assert.Eventually(t, func() bool {
		_, _, _, disconnects := persistent.counts()
		return disconnects == 1
	}, time.Second, 5*time.Millisecond)
}

func TestManagerSerializesAcquireWithFinalAndIdleDisconnect(t *testing.T) {
	for _, test := range []struct {
		name        string
		idleTimeout time.Duration
	}{
		{name: "final release"},
		{name: "idle cleanup", idleTimeout: time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			disconnectStarted := make(chan struct{})
			continueDisconnect := make(chan struct{})
			var continueOnce sync.Once
			unblockDisconnect := func() {
				continueOnce.Do(func() { close(continueDisconnect) })
			}
			defer unblockDisconnect()
			persistent := &fakePersistentManager{
				generation:      14,
				disconnectStart: disconnectStarted,
				disconnectWait:  continueDisconnect,
			}
			secondAcquireEntered := make(chan struct{})
			var runnerCalls int
			manager := NewManager(ManagerOptions{
				Persistent:  persistent,
				IdleTimeout: test.idleTimeout,
				Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
					runnerCalls++
					if runnerCalls == 2 {
						close(secondAcquireEntered)
					}
					return func(context.Context, []string) (int, error) { return 0, nil }, nil
				},
			})
			snapshot := directSnapshot("route-one")
			resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }
			lease, err := manager.Acquire(
				context.Background(),
				LeaseRequest{Snapshot: snapshot},
				resolve,
			)
			require.NoError(t, err)

			releaseDone := make(chan error, 1)
			go func() { releaseDone <- lease.Release() }()
			select {
			case <-disconnectStarted:
			case <-time.After(time.Second):
				t.Fatal("disconnect did not start")
			}

			acquireDone := make(chan error, 1)
			go func() {
				_, acquireErr := manager.Acquire(
					context.Background(),
					LeaseRequest{Snapshot: snapshot},
					resolve,
				)
				acquireDone <- acquireErr
			}()
			select {
			case <-secondAcquireEntered:
			case <-time.After(time.Second):
				t.Fatal("second acquire did not start")
			}
			time.Sleep(20 * time.Millisecond)
			connects, _, _, _ := persistent.counts()
			assert.Equal(t, 1, connects)

			unblockDisconnect()
			require.NoError(t, <-releaseDone)
			require.NoError(t, <-acquireDone)
		})
	}
}

func TestManagerReportsIdleCleanupFailure(t *testing.T) {
	persistent := &fakePersistentManager{
		generation:    16,
		disconnectErr: errors.New("exit command failed"),
	}
	events := make(chan Event, 4)
	manager := NewManager(ManagerOptions{
		Persistent:  persistent,
		IdleTimeout: 10 * time.Millisecond,
		OnEvent: func(event Event) {
			events <- event
		},
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("route-one")
	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)
	require.NoError(t, lease.Release())

	assert.Eventually(t, func() bool {
		select {
		case event := <-events:
			return event.State == EventStateError &&
				event.RouteIdentity == "route-one" &&
				event.Generation == 16 &&
				event.Failure != nil &&
				event.Failure.Code == service.SSHCleanupFailed
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)
}

func TestManagerRejectsRouteChangeAfterConnectionPreparation(t *testing.T) {
	persistent := &fakePersistentManager{generation: 12}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	expected := directSnapshot("route-one")
	changed := directSnapshot("route-two")
	resolveCalls := 0

	_, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: expected},
		func(context.Context) (RouteSnapshot, error) {
			resolveCalls++
			if resolveCalls == 1 {
				return expected, nil
			}
			return changed, nil
		},
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHConfigurationChanged))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 1, disconnects)
}

func TestManagerExecutesFreshResolvedSnapshotInsteadOfCallerFields(t *testing.T) {
	persistent := &fakePersistentManager{generation: 29}
	current := directSnapshot("route-one")
	current.Targets[0].EffectiveTarget.Hostname = "resolved.internal"
	current.Targets[0].Projection.Arguments = []string{"-F", os.DevNull, "-o", "Ciphers=resolved"}
	caller := current
	caller.Targets = append([]ResolvedTarget(nil), current.Targets...)
	caller.Targets[0].EffectiveTarget.Hostname = "injected.internal"
	caller.Targets[0].Projection.Arguments = []string{"-F", os.DevNull, "-o", "ProxyCommand=injected"}
	var runnerRequest LeaseRequest
	var runnerTarget ResolvedTarget
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(request LeaseRequest, target ResolvedTarget) (openssh.RunSSH, error) {
			runnerRequest = request
			runnerTarget = target
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})

	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: caller},
		func(context.Context) (RouteSnapshot, error) { return current, nil },
	)

	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, current, runnerRequest.Snapshot)
	assert.Equal(t, current.Targets[0], runnerTarget)
}

func TestManagerReportsCleanupFailureAfterRouteChange(t *testing.T) {
	persistent := &fakePersistentManager{
		generation:    17,
		disconnectErr: errors.New("master exit failed"),
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	expected := directSnapshot("route-one")
	resolveCalls := 0

	_, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: expected},
		func(context.Context) (RouteSnapshot, error) {
			resolveCalls++
			if resolveCalls == 1 {
				return expected, nil
			}
			return directSnapshot("route-two"), nil
		},
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHCleanupFailed))
}

func TestManagerDoesNotDisconnectSharedMasterWhenSecondLeaseChanges(t *testing.T) {
	persistent := &fakePersistentManager{generation: 15}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	expected := directSnapshot("route-one")
	first, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: expected},
		func(context.Context) (RouteSnapshot, error) { return expected, nil },
	)
	require.NoError(t, err)

	_, err = manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: expected},
		func(context.Context) (RouteSnapshot, error) {
			return directSnapshot("route-two"), nil
		},
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHConfigurationChanged))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 0, disconnects)
	require.NoError(t, first.Release())
}

func TestManagerReturnsMasterlessLeaseWhenPersistenceIsUnsupported(t *testing.T) {
	persistent := &fakePersistentManager{connectErr: openssh.ErrPersistentUnsupported}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("route-one")
	snapshot.Targets[0].StrictHostKeyChecking = "yes"

	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)

	require.NoError(t, err)
	assert.Equal(t, LeaseModeMasterless, lease.Mode())
	arguments, err := lease.Arguments(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"-F", os.DevNull, "-o", "HostName=build.internal",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ControlMaster=no", "-o", "ControlPersist=no", "-S", "none",
	}, arguments)
	require.NoError(t, lease.Touch())
	require.NoError(t, lease.Release())
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 0, disconnects)
}

func TestManagerRejectsMasterlessRouteChangeAfterProjectionPreparation(t *testing.T) {
	persistent := &fakePersistentManager{connectErr: openssh.ErrPersistentUnsupported}
	privateDirectory := filepath.Join(t.TempDir(), "private")
	manager := NewManager(ManagerOptions{
		Persistent:       persistent,
		PrivateDirectory: privateDirectory,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	expected := directSnapshot("route-one")
	expected.Targets[0].Projection.PrivateConfig = []string{`IdentityFile "C:/keys/build"`}
	resolveCalls := 0

	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: expected},
		func(context.Context) (RouteSnapshot, error) {
			resolveCalls++
			if resolveCalls == 1 {
				return expected, nil
			}
			return directSnapshot("route-two"), nil
		},
	)

	require.Error(t, err)
	assert.Nil(t, lease)
	assert.True(t, service.IsCode(err, service.SSHConfigurationChanged))
	entries, readErr := os.ReadDir(privateDirectory)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestMasterlessLeaseRetainsPrivateProjectionUntilRelease(t *testing.T) {
	persistent := &fakePersistentManager{connectErr: openssh.ErrPersistentUnsupported}
	privateDirectory := filepath.Join(t.TempDir(), "private")
	manager := NewManager(ManagerOptions{
		Persistent:       persistent,
		PrivateDirectory: privateDirectory,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("route-one")
	snapshot.Targets[0].Projection.PrivateConfig = []string{`IdentityFile "C:/keys/build"`}

	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)
	arguments, err := lease.Arguments(context.Background())
	require.NoError(t, err)
	configPath := arguments[1]
	assert.NotEqual(t, os.DevNull, configPath)
	assert.Equal(t, privateDirectory, filepath.Dir(configPath))
	contents, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, `IdentityFile "C:/keys/build"`+"\n", string(contents))

	require.NoError(t, lease.Release())
	assert.NoFileExists(t, configPath)
}

func TestManagerMapsConnectionGenerationChanges(t *testing.T) {
	persistent := &fakePersistentManager{
		generation:  13,
		argumentErr: openssh.ErrConnectionChanged,
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("route-one")

	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)

	require.NoError(t, err)
	_, err = lease.Arguments(context.Background())
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHConnectionChanged))
}
