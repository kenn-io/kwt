package ssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
	"go.kenn.io/kwt/service"
)

type fakePersistentManager struct {
	mu                        sync.Mutex
	generation                openssh.Generation
	connectErr                error
	argumentErr               error
	connectionArguments       []string
	connectCalls              int
	connectIdentities         []string
	disconnectIdentities      []string
	argumentCalls             int
	touchCalls                int
	aliveCalls                int
	aliveStart                chan struct{}
	aliveCanceled             chan struct{}
	aliveExit                 chan struct{}
	aliveExitWait             <-chan struct{}
	aliveStartOnce            sync.Once
	aliveCancelOnce           sync.Once
	aliveExitOnce             sync.Once
	disconnectCalls           int
	disconnectBeforeAliveExit bool
	disconnectErr             error
	disconnectStart           chan struct{}
	disconnectWait            <-chan struct{}
	disconnectReturn          chan struct{}
	disconnectOnce            sync.Once
	disconnectReturnOnce      sync.Once
	connectStart              chan struct{}
	connectWait               <-chan struct{}
	connectOnce               sync.Once
	connectWaitFor            string
	connectCancelCall         int
	connectCancelStarted      chan struct{}
	connectCancelOnce         sync.Once
}

func (m *fakePersistentManager) ConnectWithRunner(
	ctx context.Context,
	identity string,
	_ openssh.Target,
	_ openssh.RunSSH,
) (openssh.Generation, error) {
	m.mu.Lock()
	m.connectCalls++
	connectCall := m.connectCalls
	m.connectIdentities = append(m.connectIdentities, identity)
	generation := m.generation
	connectErr := m.connectErr
	connectStart := m.connectStart
	connectWait := m.connectWait
	connectWaitFor := m.connectWaitFor
	connectCancelCall := m.connectCancelCall
	connectCancelStarted := m.connectCancelStarted
	m.mu.Unlock()
	if connectCall == connectCancelCall {
		if connectCancelStarted != nil {
			m.connectCancelOnce.Do(func() { close(connectCancelStarted) })
		}
		<-ctx.Done()
		return generation, ctx.Err()
	}
	shouldWait := connectWait != nil && (connectWaitFor == "" || connectWaitFor == identity)
	if shouldWait && connectStart != nil {
		m.connectOnce.Do(func() { close(connectStart) })
	}
	if shouldWait {
		<-connectWait
	}
	return generation, connectErr
}

func (m *fakePersistentManager) ConnectionArguments(
	string,
	openssh.Generation,
) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.argumentCalls++
	if m.connectionArguments != nil {
		return append([]string(nil), m.connectionArguments...), m.argumentErr
	}
	return []string{"-S", "control"}, m.argumentErr
}

func (m *fakePersistentManager) TouchActivity(string, openssh.Generation) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touchCalls++
	return true
}

func (m *fakePersistentManager) IsAlive(
	ctx context.Context,
	_ string,
	_ openssh.Generation,
) (bool, error) {
	m.mu.Lock()
	m.aliveCalls++
	aliveStart := m.aliveStart
	aliveCanceled := m.aliveCanceled
	aliveExit := m.aliveExit
	aliveExitWait := m.aliveExitWait
	m.mu.Unlock()
	if aliveStart != nil {
		m.aliveStartOnce.Do(func() { close(aliveStart) })
	}
	if aliveExit != nil {
		<-ctx.Done()
		if aliveCanceled != nil {
			m.aliveCancelOnce.Do(func() { close(aliveCanceled) })
		}
		if aliveExitWait != nil {
			<-aliveExitWait
		}
		m.aliveExitOnce.Do(func() { close(aliveExit) })
		return false, ctx.Err()
	}
	return true, nil
}

func (m *fakePersistentManager) Disconnect(_ context.Context, identity string) error {
	m.mu.Lock()
	m.disconnectCalls++
	m.disconnectIdentities = append(m.disconnectIdentities, identity)
	disconnectErr := m.disconnectErr
	disconnectStart := m.disconnectStart
	disconnectWait := m.disconnectWait
	disconnectReturn := m.disconnectReturn
	if m.aliveExit != nil {
		select {
		case <-m.aliveExit:
		default:
			m.disconnectBeforeAliveExit = true
		}
	}
	m.mu.Unlock()
	if disconnectStart != nil {
		m.disconnectOnce.Do(func() { close(disconnectStart) })
	}
	if disconnectWait != nil {
		<-disconnectWait
	}
	if disconnectReturn != nil {
		m.disconnectReturnOnce.Do(func() { close(disconnectReturn) })
	}
	return disconnectErr
}

func (m *fakePersistentManager) counts() (connect, arguments, touch, disconnect int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectCalls, m.argumentCalls, m.touchCalls, m.disconnectCalls
}

func (m *fakePersistentManager) identities() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.connectIdentities...)
}

func (m *fakePersistentManager) disconnectedIdentities() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.disconnectIdentities...)
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

func proxyJumpSnapshot(identity string) RouteSnapshot {
	snapshot := directSnapshot(identity)
	snapshot.LogicalTarget = Target{Hostname: "build.example.test"}
	snapshot.Targets = []ResolvedTarget{
		{
			LogicalTarget:   Target{User: "relay", Hostname: "relay.example.test", Port: 2222},
			EffectiveTarget: Target{User: "relay", Hostname: "relay.example.test", Port: 2222},
			Projection: ExecutionProjection{Arguments: []string{
				"-F", os.DevNull, "-o", "HostName=relay.example.test", "-p", "2222",
			}},
		},
		{
			LogicalTarget:   Target{Hostname: "build.example.test"},
			EffectiveTarget: Target{User: "deploy", Hostname: "build.example.test", Port: 22},
			Projection: ExecutionProjection{Arguments: []string{
				"-F", os.DevNull, "-o", "HostName=build.example.test",
			}},
		},
	}
	return snapshot
}

func TestManagerAcquiresProxyJumpRouteAsOneCompositeLease(t *testing.T) {
	persistent := &fakePersistentManager{generation: 41}
	var prepared []ResolvedTarget
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(_ LeaseRequest, target ResolvedTarget) (openssh.RunSSH, error) {
			prepared = append(prepared, target)
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := proxyJumpSnapshot("proxy-route")
	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)
	require.Len(t, prepared, 2)
	assert.NotContains(t, prepared[0].Projection.Arguments, "ProxyCommand")
	assert.NotContains(t, prepared[1].Projection.Arguments, "ProxyJump=none")
	proxyOption := prepared[1].Projection.Arguments[len(prepared[1].Projection.Arguments)-1]
	assert.Contains(t, proxyOption, "ProxyCommand=")
	assert.Contains(t, proxyOption, "'-F' '"+os.DevNull+"'")
	assert.Contains(t, proxyOption, "'-S' 'control'")
	assert.Contains(t, proxyOption, "relay@relay.example.test")

	identities := persistent.identities()
	require.Len(t, identities, 2)
	assert.NotEqual(t, identities[0], identities[1])
	assert.Equal(t, uint64(41), lease.Generation())
	arguments, err := lease.Arguments(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"-F", os.DevNull,
		"-S", "control",
		"-o", "BatchMode=yes",
		"-o", "ProxyCommand=" + proxyFailureCommand(),
	}, arguments)
	require.NoError(t, lease.Touch())
	require.NoError(t, lease.Release(context.Background()))
	assert.Equal(t, []string{identities[1], identities[0]}, persistent.disconnectedIdentities())
}

func TestMultiplexedLeaseRetainsReviewedSessionProjection(t *testing.T) {
	privateDirectory := filepath.Join(t.TempDir(), "private")
	persistent := &fakePersistentManager{generation: 41}
	manager := NewManager(ManagerOptions{
		Persistent:       persistent,
		PrivateDirectory: privateDirectory,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := proxyJumpSnapshot("session-projection")
	snapshot.Targets[0].Projection.Arguments = append(
		snapshot.Targets[0].Projection.Arguments, "-o", "SendEnv=UPSTREAM",
	)
	snapshot.Targets[0].Projection.PrivateConfig = []string{
		`SetEnv "UPSTREAM=value"`,
	}
	snapshot.Targets[1].Projection.Arguments = append(
		snapshot.Targets[1].Projection.Arguments,
		"-o", "ForwardAgent=yes",
		"-o", "SendEnv=LANG",
		"-o", "EscapeChar=~",
	)
	snapshot.Targets[1].Projection.PrivateConfig = []string{
		`IdentityFile "/credentials/id"`,
		`SetEnv "CHANNEL=alpha beta"`,
	}

	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)

	arguments, err := lease.Arguments(context.Background())
	require.NoError(t, err)
	assert.Contains(t, arguments, "ForwardAgent=yes")
	assert.Contains(t, arguments, "SendEnv=LANG")
	assert.Contains(t, arguments, "EscapeChar=~")
	assert.NotContains(t, arguments, "SendEnv=UPSTREAM")
	configIndex := slices.Index(arguments, "-F")
	require.GreaterOrEqual(t, configIndex, 0)
	require.Less(t, configIndex+1, len(arguments))
	configPath := arguments[configIndex+1]
	assert.NotEqual(t, os.DevNull, configPath)
	config, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, "SetEnv \"CHANNEL=alpha beta\"\n", string(config))

	require.NoError(t, lease.Release(context.Background()))
	_, err = os.Stat(configPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestManagerCloseDisconnectsProxyRouteDownstreamFirst(t *testing.T) {
	persistent := &fakePersistentManager{generation: 41}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := proxyJumpSnapshot("proxy-close-order")
	_, err := manager.Acquire(
		context.Background(), LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)
	identities := persistent.identities()
	require.Len(t, identities, 2)

	require.NoError(t, manager.Close(context.Background()))
	assert.Equal(t, []string{identities[1], identities[0]}, persistent.disconnectedIdentities())
}

func TestManagerIdleCleanupWaitsForDownstreamProxyTeardown(t *testing.T) {
	disconnectStarted := make(chan struct{})
	allowDisconnect := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(allowDisconnect) }) }
	defer unblock()
	persistent := &fakePersistentManager{
		generation: 41, disconnectStart: disconnectStarted, disconnectWait: allowDisconnect,
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent, IdleTimeout: time.Millisecond,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := proxyJumpSnapshot("proxy-idle-order")
	lease, err := manager.Acquire(
		context.Background(), LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)
	identities := persistent.identities()
	require.Len(t, identities, 2)
	require.NoError(t, lease.Release(context.Background()))

	select {
	case <-disconnectStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "downstream idle cleanup did not start")
	}
	assert.Equal(t, []string{identities[1]}, persistent.disconnectedIdentities())
	assert.Never(t, func() bool {
		return len(persistent.disconnectedIdentities()) > 1
	}, 30*time.Millisecond, time.Millisecond)
	unblock()
	require.Eventually(t, func() bool {
		return len(persistent.disconnectedIdentities()) == 2
	}, time.Second, time.Millisecond)
	assert.Equal(t, []string{identities[1], identities[0]}, persistent.disconnectedIdentities())
}

func TestManagerPartitionsEveryProxyHopByForwardedAgentChain(t *testing.T) {
	persistent := &fakePersistentManager{generation: 42}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := proxyJumpSnapshot("proxy-agent-route")
	snapshot.Targets[0].ForwardAgent = true
	resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }

	first, err := manager.Acquire(context.Background(), LeaseRequest{
		Snapshot: snapshot, Environment: []string{"SSH_AUTH_SOCK=/tmp/agent-one"},
	}, resolve)
	require.NoError(t, err)
	second, err := manager.Acquire(context.Background(), LeaseRequest{
		Snapshot: snapshot, Environment: []string{"SSH_AUTH_SOCK=/tmp/agent-two"},
	}, resolve)
	require.NoError(t, err)

	identities := persistent.identities()
	require.Len(t, identities, 4)
	assert.NotEqual(t, identities[0], identities[2])
	assert.NotEqual(t, identities[1], identities[3])
	require.NoError(t, first.Release(context.Background()))
	require.NoError(t, second.Release(context.Background()))
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
	assert.Equal(t, []string{
		"-F", os.DevNull,
		"-S", "control",
		"-o", "BatchMode=yes",
		"-o", "ProxyCommand=" + proxyFailureCommand(),
	}, arguments)
	require.NoError(t, first.Touch())
	require.NoError(t, first.Release(context.Background()))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 0, disconnects)
	require.NoError(t, second.Release(context.Background()))
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

	require.NoError(t, lease.Release(context.Background()))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 0, disconnects)
	assert.Eventually(t, func() bool {
		_, _, _, disconnects := persistent.counts()
		return disconnects == 1
	}, time.Second, 5*time.Millisecond)
}

func TestManagerDisconnectsForwardAgentRouteOnFinalRelease(t *testing.T) {
	persistent := &fakePersistentManager{generation: 15}
	manager := NewManager(ManagerOptions{
		Persistent:  persistent,
		IdleTimeout: time.Hour,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("forwarded-agent-route")
	snapshot.Targets[0].ForwardAgent = true
	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)

	require.NoError(t, lease.Release(context.Background()))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 1, disconnects)
}

func TestManagerPartitionsForwardAgentRoutesByExecutionEnvironment(t *testing.T) {
	persistent := &fakePersistentManager{generation: 19}
	manager := NewManager(ManagerOptions{
		Persistent:  persistent,
		IdleTimeout: time.Hour,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("forwarded-agent-route")
	snapshot.Targets[0].ForwardAgent = true
	resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }

	first, err := manager.Acquire(
		context.Background(),
		LeaseRequest{
			Snapshot: snapshot,
			Environment: []string{
				"SSH_AUTH_SOCK=/tmp/default-agent",
				"AGENT_SOCKET=/tmp/agent-one",
			},
		},
		resolve,
	)
	require.NoError(t, err)
	second, err := manager.Acquire(
		context.Background(),
		LeaseRequest{
			Snapshot: snapshot,
			Environment: []string{
				"SSH_AUTH_SOCK=/tmp/default-agent",
				"AGENT_SOCKET=/tmp/agent-two",
			},
		},
		resolve,
	)
	require.NoError(t, err)

	identities := persistent.identities()
	require.Len(t, identities, 2)
	assert.NotEqual(t, identities[0], identities[1])
	require.NoError(t, first.Release(context.Background()))
	require.NoError(t, second.Release(context.Background()))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 2, disconnects)
}

func TestManagerRefreshesOpenSSHPersistenceWhileRouteIsTracked(t *testing.T) {
	persistent := &fakePersistentManager{generation: 16}
	manager := NewManager(ManagerOptions{
		Persistent:         persistent,
		IdleTimeout:        time.Hour,
		PersistenceTimeout: 30 * time.Millisecond,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("persistent-route")
	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		persistent.mu.Lock()
		defer persistent.mu.Unlock()
		return persistent.aliveCalls > 0
	}, time.Second, 5*time.Millisecond)
	persistent.mu.Lock()
	activeHeartbeats := persistent.aliveCalls
	persistent.mu.Unlock()
	require.NoError(t, lease.Release(context.Background()))
	assert.Eventually(t, func() bool {
		persistent.mu.Lock()
		defer persistent.mu.Unlock()
		return persistent.aliveCalls > activeHeartbeats
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, manager.Close(context.Background()))
}

func TestManagerWaitsForPersistenceHeartbeatBeforeTeardown(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(*Manager, Lease) error
	}{
		{name: "final release", stop: func(_ *Manager, lease Lease) error {
			return lease.Release(context.Background())
		}},
		{name: "manager close", stop: func(manager *Manager, _ Lease) error {
			return manager.Close(context.Background())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			aliveStarted := make(chan struct{})
			aliveCanceled := make(chan struct{})
			aliveExited := make(chan struct{})
			allowAliveExit := make(chan struct{})
			persistent := &fakePersistentManager{
				generation:    18,
				aliveStart:    aliveStarted,
				aliveCanceled: aliveCanceled,
				aliveExit:     aliveExited,
				aliveExitWait: allowAliveExit,
			}
			manager := NewManager(ManagerOptions{
				Persistent:         persistent,
				PersistenceTimeout: 30 * time.Millisecond,
				Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
					return func(context.Context, []string) (int, error) { return 0, nil }, nil
				},
			})
			snapshot := directSnapshot("route-with-active-heartbeat")
			lease, err := manager.Acquire(
				context.Background(),
				LeaseRequest{Snapshot: snapshot},
				func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
			)
			require.NoError(t, err)
			select {
			case <-aliveStarted:
			case <-time.After(time.Second):
				t.Fatal("persistence heartbeat did not start")
			}

			stopDone := make(chan error, 1)
			go func() { stopDone <- test.stop(manager, lease) }()
			select {
			case <-aliveCanceled:
			case <-time.After(time.Second):
				t.Fatal("teardown did not cancel the persistence heartbeat")
			}
			select {
			case <-stopDone:
				t.Fatal("teardown returned before the persistence heartbeat exited")
			default:
			}
			persistent.mu.Lock()
			disconnects := persistent.disconnectCalls
			persistent.mu.Unlock()
			assert.Equal(t, 0, disconnects)
			close(allowAliveExit)
			require.NoError(t, <-stopDone)
			select {
			case <-aliveExited:
			default:
				t.Fatal("teardown returned before the persistence heartbeat exited")
			}
			persistent.mu.Lock()
			disconnectedEarly := persistent.disconnectBeforeAliveExit
			persistent.mu.Unlock()
			assert.False(t, disconnectedEarly)
		})
	}
}

func TestManagerRetriesFailedFinalAndIdleCleanupOnClose(t *testing.T) {
	for _, test := range []struct {
		name        string
		idleTimeout time.Duration
	}{
		{name: "final release"},
		{name: "idle cleanup", idleTimeout: time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			persistent := &fakePersistentManager{
				generation:    17,
				disconnectErr: errors.New("exit command failed"),
			}
			manager := NewManager(ManagerOptions{
				Persistent:  persistent,
				IdleTimeout: test.idleTimeout,
				Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
					return func(context.Context, []string) (int, error) { return 0, nil }, nil
				},
			})
			snapshot := directSnapshot("route-with-failed-cleanup")
			lease, err := manager.Acquire(
				context.Background(),
				LeaseRequest{Snapshot: snapshot},
				func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
			)
			require.NoError(t, err)

			releaseErr := lease.Release(context.Background())
			if test.idleTimeout == 0 {
				require.Error(t, releaseErr)
			} else {
				require.NoError(t, releaseErr)
				assert.Eventually(t, func() bool {
					_, _, _, disconnects := persistent.counts()
					return disconnects == 1
				}, time.Second, 5*time.Millisecond)
			}
			persistent.mu.Lock()
			persistent.disconnectErr = nil
			persistent.mu.Unlock()

			require.NoError(t, manager.Close(context.Background()))
			_, _, _, disconnects := persistent.counts()
			assert.Equal(t, 2, disconnects)
		})
	}
}

func TestLeaseReleaseRetriesFailedDisconnect(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot RouteSnapshot
		hops     int
	}{
		{name: "direct route", snapshot: directSnapshot("direct-release-retry"), hops: 1},
		{name: "proxy route", snapshot: proxyJumpSnapshot("proxy-release-retry"), hops: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			persistent := &fakePersistentManager{
				generation:    17,
				disconnectErr: errors.New("exit command failed"),
			}
			manager := NewManager(ManagerOptions{
				Persistent: persistent,
				Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
					return func(context.Context, []string) (int, error) { return 0, nil }, nil
				},
			})
			lease, err := manager.Acquire(
				context.Background(),
				LeaseRequest{Snapshot: test.snapshot},
				func(context.Context) (RouteSnapshot, error) { return test.snapshot, nil },
			)
			require.NoError(t, err)

			require.Error(t, lease.Release(context.Background()))
			persistent.mu.Lock()
			persistent.disconnectErr = nil
			persistent.mu.Unlock()
			require.NoError(t, lease.Release(context.Background()))

			_, _, _, disconnects := persistent.counts()
			assert.Equal(t, 2*test.hops, disconnects)
			manager.mu.Lock()
			assert.Empty(t, manager.routes)
			manager.mu.Unlock()
		})
	}
}

func TestProxyAcquireCancellationReleasesEarlierHops(t *testing.T) {
	secondHopStarted := make(chan struct{})
	persistent := &fakePersistentManager{
		generation:           17,
		connectCancelCall:    2,
		connectCancelStarted: secondHopStarted,
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := proxyJumpSnapshot("proxy-cancel-rollback")
	ctx, cancel := context.WithCancel(context.Background())
	acquireDone := make(chan error, 1)
	go func() {
		_, err := manager.Acquire(
			ctx,
			LeaseRequest{Snapshot: snapshot},
			func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
		)
		acquireDone <- err
	}()
	select {
	case <-secondHopStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "second ProxyJump hop did not start")
	}
	cancel()

	assert.ErrorIs(t, <-acquireDone, context.Canceled)
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 1, disconnects)
	manager.mu.Lock()
	assert.Empty(t, manager.routes)
	manager.mu.Unlock()
}

func TestLeaseReleaseCompletesAfterFailedRouteIsReplaced(t *testing.T) {
	persistent := &fakePersistentManager{
		generation:    17,
		disconnectErr: errors.New("exit command failed"),
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("release-after-replacement")
	resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }
	oldLease, err := manager.Acquire(
		context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
	)
	require.NoError(t, err)
	require.Error(t, oldLease.Release(context.Background()))

	persistent.mu.Lock()
	persistent.generation = 18
	persistent.disconnectErr = nil
	persistent.mu.Unlock()
	newLease, err := manager.Acquire(
		context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
	)
	require.NoError(t, err)

	require.NoError(t, oldLease.Release(context.Background()))
	require.NoError(t, newLease.Release(context.Background()))
}

func TestStaleLeaseFirstReleaseCompletesAfterRouteGenerationIsReplaced(t *testing.T) {
	persistent := &fakePersistentManager{generation: 17}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("release-stale-generation")
	resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }
	oldLease, err := manager.Acquire(
		context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
	)
	require.NoError(t, err)

	persistent.mu.Lock()
	persistent.generation = 18
	persistent.mu.Unlock()
	newLease, err := manager.Acquire(
		context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
	)
	require.NoError(t, err)

	require.NoError(t, oldLease.Release(context.Background()))
	require.NoError(t, newLease.Release(context.Background()))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 1, disconnects)
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
			go func() { releaseDone <- lease.Release(context.Background()) }()
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

func TestManagerCloseDisconnectsRoutesAndRejectsNewAcquires(t *testing.T) {
	persistent := &fakePersistentManager{generation: 31}
	manager := NewManager(ManagerOptions{
		Persistent:  persistent,
		IdleTimeout: time.Hour,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("route-one")
	resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }
	lease, err := manager.Acquire(
		context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
	)
	require.NoError(t, err)

	require.NoError(t, manager.Close(context.Background()))
	_, err = manager.Acquire(
		context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHConnectionChanged))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 1, disconnects)
	require.NoError(t, lease.Release(context.Background()))
}

func TestManagerCloseSerializesWithFinalLeaseRelease(t *testing.T) {
	disconnectStarted := make(chan struct{})
	continueDisconnect := make(chan struct{})
	disconnectReturned := make(chan struct{})
	persistent := &fakePersistentManager{
		generation:       35,
		disconnectStart:  disconnectStarted,
		disconnectWait:   continueDisconnect,
		disconnectReturn: disconnectReturned,
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
	identity := managerIdentity(snapshot)

	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close(context.Background()) }()
	select {
	case <-disconnectStarted:
	case <-time.After(time.Second):
		t.Fatal("manager close did not begin disconnecting")
	}
	manager.mu.Lock()
	operation := manager.operations[identity]
	require.NotNil(t, operation)
	identityReleased := make(chan struct{})
	go func() {
		<-operation.gate
		close(identityReleased)
		operation.gate <- struct{}{}
	}()
	close(continueDisconnect)
	select {
	case <-disconnectReturned:
	case <-time.After(time.Second):
		manager.mu.Unlock()
		t.Fatal("disconnect did not return")
	}
	select {
	case <-identityReleased:
		manager.mu.Unlock()
		t.Fatal("manager released the route identity before deleting the route")
	case <-time.After(20 * time.Millisecond):
	}
	manager.mu.Unlock()

	require.NoError(t, <-closeDone)
	require.NoError(t, lease.Release(context.Background()))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 1, disconnects)
}

func TestManagerCloseHonorsContextWhileAcquireOwnsIdentity(t *testing.T) {
	connectStarted := make(chan struct{})
	continueConnect := make(chan struct{})
	var continueOnce sync.Once
	unblockConnect := func() {
		continueOnce.Do(func() { close(continueConnect) })
	}
	defer unblockConnect()
	persistent := &fakePersistentManager{
		generation:   33,
		connectStart: connectStarted,
		connectWait:  continueConnect,
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("route-one")
	resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }
	acquireDone := make(chan error, 1)
	go func() {
		_, err := manager.Acquire(
			context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
		)
		acquireDone <- err
	}()
	select {
	case <-connectStarted:
	case <-time.After(time.Second):
		t.Fatal("connection preparation did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close(ctx) }()
	select {
	case err := <-closeDone:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(200 * time.Millisecond):
		unblockConnect()
		t.Fatal("manager close ignored its context deadline")
	}

	unblockConnect()
	assert.True(t, service.IsCode(<-acquireDone, service.SSHConnectionChanged))
}

func TestManagerCloseCleansEstablishedRoutesBeforeWaitingOnOperations(t *testing.T) {
	connectStarted := make(chan struct{})
	continueConnect := make(chan struct{})
	var continueOnce sync.Once
	unblockConnect := func() {
		continueOnce.Do(func() { close(continueConnect) })
	}
	defer unblockConnect()
	established := directSnapshot("zzz-established")
	blocked := directSnapshot("aaa-blocked")
	persistent := &fakePersistentManager{
		generation:     34,
		connectStart:   connectStarted,
		connectWait:    continueConnect,
		connectWaitFor: managerIdentity(blocked),
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	establishedLease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: established},
		func(context.Context) (RouteSnapshot, error) { return established, nil },
	)
	require.NoError(t, err)
	blockedDone := make(chan error, 1)
	go func() {
		_, acquireErr := manager.Acquire(
			context.Background(),
			LeaseRequest{Snapshot: blocked},
			func(context.Context) (RouteSnapshot, error) { return blocked, nil },
		)
		blockedDone <- acquireErr
	}()
	select {
	case <-connectStarted:
	case <-time.After(time.Second):
		t.Fatal("blocked connection preparation did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, manager.Close(ctx), context.DeadlineExceeded)
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 1, disconnects)

	unblockConnect()
	assert.True(t, service.IsCode(<-blockedDone, service.SSHConnectionChanged))
	require.NoError(t, manager.Close(context.Background()))
	require.NoError(t, establishedLease.Release(context.Background()))
}

func TestManagerPreservesTypedPromptFailure(t *testing.T) {
	promptFailure := service.NewError(
		service.SSHPromptRejected,
		"SSH prompt was rejected",
		false,
		nil,
		nil,
	)
	persistent := &fakePersistentManager{
		connectErr: errors.Join(errors.New("ssh exited"), promptFailure),
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 1, promptFailure }, nil
		},
	})
	snapshot := directSnapshot("route-one")

	_, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHPromptRejected))
	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.False(t, typed.Retryable)
}

func TestManagerEventCallbackCanAcquireSameRoute(t *testing.T) {
	persistent := &fakePersistentManager{generation: 32}
	snapshot := directSnapshot("route-one")
	resolve := func(context.Context) (RouteSnapshot, error) { return snapshot, nil }
	callbackDone := make(chan error, 1)
	var callbackOnce sync.Once
	var manager *Manager
	manager = NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
		OnEvent: func(event Event) {
			if event.State != EventStateConnected {
				return
			}
			callbackOnce.Do(func() {
				lease, err := manager.Acquire(
					context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
				)
				if err == nil {
					err = lease.Release(context.Background())
				}
				callbackDone <- err
			})
		},
	})
	acquireDone := make(chan error, 1)
	go func() {
		_, err := manager.Acquire(
			context.Background(), LeaseRequest{Snapshot: snapshot}, resolve,
		)
		acquireDone <- err
	}()

	select {
	case err := <-callbackDone:
		require.NoError(t, err)
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "event callback deadlocked while acquiring the same route")
		return
	}
	require.NoError(t, <-acquireDone)
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
	require.NoError(t, lease.Release(context.Background()))

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

func TestManagerFullyTearsDownWarmRouteAfterPostConnectChange(t *testing.T) {
	persistent := &fakePersistentManager{generation: 20}
	events := make(chan Event, 4)
	manager := NewManager(ManagerOptions{
		Persistent:  persistent,
		IdleTimeout: time.Hour,
		OnEvent: func(event Event) {
			events <- event
		},
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	expected := directSnapshot("route-one")
	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: expected},
		func(context.Context) (RouteSnapshot, error) { return expected, nil },
	)
	require.NoError(t, err)
	require.NoError(t, lease.Release(context.Background()))

	resolveCalls := 0
	_, err = manager.Acquire(
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
	assert.True(t, service.IsCode(err, service.SSHConfigurationChanged))
	identity := managerIdentity(expected)
	manager.mu.Lock()
	_, tracked := manager.routes[identity]
	manager.mu.Unlock()
	assert.False(t, tracked)
	require.NoError(t, manager.Close(context.Background()))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 1, disconnects)

	var disconnected int
	close(events)
	for event := range events {
		if event.State == EventStateDisconnected && event.RouteIdentity == expected.RouteIdentity {
			disconnected++
		}
	}
	assert.Equal(t, 1, disconnected)
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
	identity := managerIdentity(expected)
	manager.mu.Lock()
	_, tracked := manager.routes[identity]
	manager.mu.Unlock()
	assert.True(t, tracked)
	persistent.mu.Lock()
	persistent.disconnectErr = nil
	persistent.mu.Unlock()
	require.NoError(t, manager.Close(context.Background()))
	_, _, _, disconnects := persistent.counts()
	assert.Equal(t, 2, disconnects)
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
	require.NoError(t, first.Release(context.Background()))
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
	require.NoError(t, lease.Release(context.Background()))
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

	require.NoError(t, lease.Release(context.Background()))
	assert.NoFileExists(t, configPath)
}

func TestManagerCloseCleansMasterlessPrivateProjection(t *testing.T) {
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
	require.FileExists(t, configPath)

	require.NoError(t, manager.Close(context.Background()))

	assert.NoFileExists(t, configPath)
	_, err = lease.Arguments(context.Background())
	assert.True(t, service.IsCode(err, service.SSHConnectionChanged))
}

func TestMasterlessLeaseRetriesFailedCleanupOnClose(t *testing.T) {
	manager := NewManager(ManagerOptions{})
	cleanupCalls := 0
	lease := &masterlessLease{
		generation:    41,
		manager:       manager,
		routeIdentity: "masterless-route",
		cleanup: func() error {
			cleanupCalls++
			if cleanupCalls == 1 {
				return errors.New("projection cleanup failed")
			}
			return nil
		},
	}
	manager.masterless[lease.generation] = lease

	require.Error(t, lease.Release(context.Background()))
	require.NoError(t, manager.Close(context.Background()))
	assert.Equal(t, 2, cleanupCalls)
}

func TestMasterlessReleaseAndCloseShareOneCleanupAttempt(t *testing.T) {
	cleanupStarted := make(chan struct{})
	allowCleanup := make(chan struct{})
	var cleanupCalls atomic.Int32
	events := make(chan Event, 2)
	manager := NewManager(ManagerOptions{OnEvent: func(event Event) { events <- event }})
	lease := &masterlessLease{
		generation:    42,
		manager:       manager,
		routeIdentity: "masterless-route",
		cleanup: func() error {
			if cleanupCalls.Add(1) == 1 {
				close(cleanupStarted)
			}
			<-allowCleanup
			return nil
		},
	}
	manager.masterless[lease.generation] = lease

	releaseDone := make(chan error, 1)
	go func() { releaseDone <- lease.Release(context.Background()) }()
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("lease release did not begin cleanup")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close(context.Background()) }()
	assert.Never(t, func() bool { return cleanupCalls.Load() > 1 }, 20*time.Millisecond, time.Millisecond)
	close(allowCleanup)

	require.NoError(t, <-releaseDone)
	require.NoError(t, <-closeDone)
	assert.Equal(t, int32(1), cleanupCalls.Load())
	close(events)
	var disconnected int
	for event := range events {
		if event.State == EventStateDisconnected {
			disconnected++
		}
	}
	assert.Equal(t, 1, disconnected)
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
