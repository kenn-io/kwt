package ssh

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
	servicepkg "go.kenn.io/kwt/service"
)

type recordingPublicResolver struct{}

func (recordingPublicResolver) Resolve(
	context.Context,
	ResolveRequest,
) (RouteSnapshot, error) {
	return RouteSnapshot{}, nil
}

type fixedPublicResolver struct {
	snapshot RouteSnapshot
}

func (r fixedPublicResolver) Resolve(
	context.Context,
	ResolveRequest,
) (RouteSnapshot, error) {
	return r.snapshot, nil
}

type publicTestPersistentManager struct{}

func (publicTestPersistentManager) ConnectWithRunner(
	context.Context,
	string,
	openssh.Target,
	openssh.RunSSH,
) (openssh.Generation, error) {
	return 23, nil
}

func (publicTestPersistentManager) ConnectionArguments(
	string,
	openssh.Generation,
) ([]string, error) {
	return []string{"-S", "control"}, nil
}

func (publicTestPersistentManager) TouchActivity(string, openssh.Generation) bool { return true }
func (publicTestPersistentManager) IsAlive(context.Context, string, openssh.Generation) (bool, error) {
	return true, nil
}
func (publicTestPersistentManager) Disconnect(context.Context, string) error { return nil }

func TestPublicServiceReloadsConfiguredProtectedEnvironmentPerRequest(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(
		configPath,
		[]byte("[fleet]\ntoken_env = \"TOKEN_ONE\"\n"),
		0o600,
	))
	environment := []string{
		"TOKEN_ONE=first-secret",
		"TOKEN_TWO=second-secret",
		"SAFE=value",
	}
	var captured []ResolverOptions
	service := NewPublicService(PublicServiceOptions{
		Home:        home,
		Environment: func() []string { return append([]string(nil), environment...) },
	})
	service.build = func(options ResolverOptions) snapshotResolver {
		captured = append(captured, options)
		return recordingPublicResolver{}
	}

	_, err := service.Resolve(context.Background(), ResolveRequest{})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		configPath,
		[]byte("[fleet]\ntoken_env = \"TOKEN_TWO\"\n"),
		0o600,
	))
	_, err = service.Resolve(context.Background(), ResolveRequest{})
	require.NoError(t, err)
	require.Len(t, captured, 2)
	assert.NotContains(t, captured[0].Environment, "TOKEN_ONE=first-secret")
	assert.Contains(t, captured[0].Environment, "TOKEN_TWO=second-secret")
	assert.NotContains(t, captured[1].Environment, "TOKEN_TWO=second-secret")
	assert.Contains(t, captured[1].Environment, "TOKEN_ONE=first-secret")
}

func TestPublicServiceUsesRequestEnvironmentAndStripsConfiguredCredentials(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(
		configPath,
		[]byte("[fleet]\ntoken_env = \"GHOSTHUB_AUTH\"\n"),
		0o600,
	))
	var captured ResolverOptions
	service := NewPublicService(PublicServiceOptions{
		Home:        home,
		Environment: func() []string { return []string{"PATH=/daemon/bin"} },
	})
	service.build = func(options ResolverOptions) snapshotResolver {
		captured = options
		return recordingPublicResolver{}
	}

	_, err := service.Resolve(context.Background(), ResolveRequest{
		Environment: []string{
			"PATH=/invocation/bin",
			"SSH_AUTH_SOCK=/invocation/agent.sock",
			"GHOSTHUB_AUTH=fleet-secret",
		},
	})
	require.NoError(t, err)
	assert.False(t, slices.Contains(captured.Environment, "PATH=/daemon/bin"))
	assert.Contains(t, captured.Environment, "PATH=/invocation/bin")
	assert.Contains(t, captured.Environment, "SSH_AUTH_SOCK=/invocation/agent.sock")
	assert.NotContains(t, captured.Environment, "GHOSTHUB_AUTH=fleet-secret")
}

func TestPublicServiceAcquiresLeaseThroughSharedManager(t *testing.T) {
	snapshot := directSnapshot("route-one")
	service := NewPublicService(PublicServiceOptions{})
	service.build = func(ResolverOptions) snapshotResolver {
		return fixedPublicResolver{snapshot: snapshot}
	}
	service.leases = NewManager(ManagerOptions{
		Persistent: publicTestPersistentManager{},
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})

	lease, err := service.Acquire(context.Background(), LeaseRequest{Snapshot: snapshot})

	require.NoError(t, err)
	assert.Equal(t, uint64(23), lease.Generation())
}

func TestPublicServiceRequiresExplicitAskpassExecutableForAcquire(t *testing.T) {
	home := t.TempDir()
	snapshot := directSnapshot("route-one")
	service := NewPublicService(PublicServiceOptions{Home: home})
	service.build = func(ResolverOptions) snapshotResolver {
		return fixedPublicResolver{snapshot: snapshot}
	}
	service.newPersistent = func(
		string,
		openssh.PersistentConfig,
	) (PersistentManager, error) {
		return publicTestPersistentManager{}, nil
	}

	_, err := service.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
	)

	require.Error(t, err)
	assert.True(t, servicepkg.IsCode(err, servicepkg.SSHConnectionFailed))
}

func TestPublicServiceBuildsOneOwnerScopedPersistentManager(t *testing.T) {
	home := t.TempDir()
	snapshot := directSnapshot("route-one")
	service := NewPublicService(PublicServiceOptions{
		Home:              home,
		AskpassExecutable: filepath.Join(home, "kwt"),
	})
	service.build = func(ResolverOptions) snapshotResolver {
		return fixedPublicResolver{snapshot: snapshot}
	}
	created := 0
	controlDirectory := ""
	var persistentConfig openssh.PersistentConfig
	service.newPersistent = func(
		directory string,
		config openssh.PersistentConfig,
	) (PersistentManager, error) {
		created++
		controlDirectory = directory
		persistentConfig = config
		require.NoError(t, os.MkdirAll(directory, 0o700))
		return publicTestPersistentManager{}, nil
	}

	first, err := service.Acquire(context.Background(), LeaseRequest{Snapshot: snapshot})
	require.NoError(t, err)
	second, err := service.Acquire(context.Background(), LeaseRequest{Snapshot: snapshot})
	require.NoError(t, err)
	require.NoError(t, first.Release())
	require.NoError(t, second.Release())
	require.NoError(t, service.Close(context.Background()))

	assert.Equal(t, 1, created)
	assert.True(t, strings.HasPrefix(
		controlDirectory,
		filepath.Join(home, "runtime")+string(filepath.Separator)+"ssh-",
	))
	assert.Equal(t, "control", filepath.Base(controlDirectory))
	require.NotNil(t, persistentConfig.ConnectionOptions)
	assert.Equal(t, time.Hour, persistentConfig.ConnectionOptions.ControlPersistTimeout)
	assert.NoDirExists(t, filepath.Dir(controlDirectory))
}

func TestPublicServiceClosePreventsManagerReinitialization(t *testing.T) {
	home := t.TempDir()
	snapshot := directSnapshot("route-one")
	service := NewPublicService(PublicServiceOptions{Home: home})
	service.build = func(ResolverOptions) snapshotResolver {
		return fixedPublicResolver{snapshot: snapshot}
	}
	created := 0
	service.newPersistent = func(
		string,
		openssh.PersistentConfig,
	) (PersistentManager, error) {
		created++
		return publicTestPersistentManager{}, nil
	}

	require.NoError(t, service.Close(context.Background()))
	_, err := service.Acquire(context.Background(), LeaseRequest{Snapshot: snapshot})

	require.Error(t, err)
	assert.True(t, servicepkg.IsCode(err, servicepkg.SSHConnectionChanged))
	assert.Equal(t, 0, created)
}
