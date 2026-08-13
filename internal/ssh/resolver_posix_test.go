//go:build !windows

package ssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestResolverPOSIXUsesLoginShellFrameAndExplicitTarget(t *testing.T) {
	var gotArgv, gotEnvironment []string
	resolver := NewResolver(ResolverOptions{
		Executable: "/usr/bin/ssh",
		LoginShell: func() (string, error) { return "/bin/zsh", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
		Environment: []string{
			"HOME=/Users/operator", "PATH=/usr/bin:/bin",
			"KWT_GITHUB_TOKEN=github-secret", "GHOSTHUB_AUTH=fleet-secret",
		},
		ProtectedNames: []string{"GHOSTHUB_AUTH"},
		Run: func(_ context.Context, argv []string, _ string, environment []string, _ []byte) ([]byte, []byte, int, error) {
			gotArgv = append([]string(nil), argv...)
			gotEnvironment = append([]string(nil), environment...)
			return []byte("banner\nKWT_SSH_CONFIG_START_nonce\n" +
					"user deploy\nhostname build.internal\nport 2200\n" +
					"KWT_SSH_CONFIG_END_nonce\ntrailer\n"),
				[]byte("KWT_SSH_CONFIG_START_nonce is stderr only"), 0, nil
		},
	})

	observation, err := resolver.Resolve(context.Background(), ResolveRequest{Target: Target{
		User: "deploy", Hostname: "build.example.test", Port: 2200,
	}})
	require.NoError(t, err)
	require.Len(t, observation.route, 1)
	assert.Equal(t, "build.internal", observation.route[0].Config.Hostname)
	assert.Equal(t, []string{"/bin/zsh", "-l", "-c"}, gotArgv[:3])
	assert.Equal(t, `exec /bin/sh -c "$KWT_SSH_RESOLVE_COMMAND"`, gotArgv[3])
	assert.Contains(t, resolveCommandFromEnvironment(t, gotEnvironment),
		"'/usr/bin/ssh' '-G' '-l' 'deploy' '-p' '2200' '--' 'build.example.test'")
	assert.Contains(t, gotEnvironment, "HOME=/Users/operator")
	assert.Contains(t, gotEnvironment, "PATH=/usr/bin:/bin")
	assert.NotContains(t, gotEnvironment, "KWT_GITHUB_TOKEN=github-secret")
	assert.NotContains(t, gotEnvironment, "GHOSTHUB_AUTH=fleet-secret")
}

func TestResolverPOSIXBindsBareExecutableBeforeLoginShellStartup(t *testing.T) {
	workingDirectory := t.TempDir()
	binDirectory := filepath.Join(workingDirectory, "tools")
	require.NoError(t, os.Mkdir(binDirectory, 0o700))
	executable := filepath.Join(binDirectory, "ssh")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	var command string
	resolver := NewResolver(ResolverOptions{
		Executable:       "ssh",
		WorkingDirectory: workingDirectory,
		LoginShell:       func() (string, error) { return "/bin/sh", nil },
		Nonce:            func() (string, error) { return "nonce", nil },
		Environment:      []string{"PATH=tools"},
		Run: func(_ context.Context, _ []string, _ string, environment []string, _ []byte) ([]byte, []byte, int, error) {
			command = resolveCommandFromEnvironment(t, environment)
			return framedResolverOutput("nonce", "hostname build.internal\n"), nil, 0, nil
		},
	})

	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.NoError(t, err)
	assert.Contains(t, command, shellQuote(executable)+" '-G'")
	assert.NotContains(t, command, "'ssh' '-G'")
}

func TestResolverPOSIXRestoresWorkingDirectoryAfterLoginShellStartup(t *testing.T) {
	workingDirectory := t.TempDir()
	startupDirectory := t.TempDir()
	capturePath := filepath.Join(t.TempDir(), "cwd")
	shellPath := filepath.Join(t.TempDir(), "login-shell")
	require.NoError(t, os.WriteFile(shellPath, []byte(`#!/bin/sh
cd "$KWT_TEST_STARTUP_DIR" || exit 1
exec /bin/sh -c "$3"
`), 0o700))
	sshPath := filepath.Join(t.TempDir(), "ssh")
	require.NoError(t, os.WriteFile(sshPath, []byte(`#!/bin/sh
printf '%s' "$PWD" > "$KWT_TEST_CAPTURE"
printf 'hostname build.internal\n'
`), 0o700))
	resolver := NewResolver(ResolverOptions{
		Executable:       sshPath,
		WorkingDirectory: workingDirectory,
		LoginShell:       func() (string, error) { return shellPath, nil },
		Environment: []string{
			"PATH=/usr/bin:/bin",
			"KWT_TEST_STARTUP_DIR=" + startupDirectory,
			"KWT_TEST_CAPTURE=" + capturePath,
		},
	})

	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.NoError(t, err)
	captured, err := os.ReadFile(capturePath)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(workingDirectory)
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(string(captured))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestResolverPOSIXDelegatesResolveScriptThroughPOSIXShellForFish(t *testing.T) {
	var gotArgv []string
	resolver := NewResolver(ResolverOptions{
		LoginShell: func() (string, error) { return "/usr/local/bin/fish", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
		Run: func(_ context.Context, argv []string, _ string, _ []string, _ []byte) ([]byte, []byte, int, error) {
			gotArgv = append([]string(nil), argv...)
			return framedResolverOutput("nonce", "hostname build.internal\n"), nil, 0, nil
		},
	})

	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.NoError(t, err)
	require.Len(t, gotArgv, 4)
	assert.Equal(t, []string{"/usr/local/bin/fish", "-l", "-c"}, gotArgv[:3])
	assert.Equal(t, `exec /bin/sh -c "$KWT_SSH_RESOLVE_COMMAND"`, gotArgv[3])
}

func TestResolverPOSIXDelegatesResolveScriptThroughPOSIXShellForTcsh(t *testing.T) {
	var gotArgv, gotStandardInput []string
	resolver := NewResolver(ResolverOptions{
		LoginShell: func() (string, error) { return "/bin/tcsh", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
		Run: func(_ context.Context, argv []string, _ string, _ []string, standardInput []byte) ([]byte, []byte, int, error) {
			gotArgv = append([]string(nil), argv...)
			gotStandardInput = append(gotStandardInput, string(standardInput))
			return framedResolverOutput("nonce", "hostname build.internal\n"), nil, 0, nil
		},
	})

	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/bin/tcsh", "-l"}, gotArgv)
	assert.Equal(t, []string{`exec /bin/sh -c "$KWT_SSH_RESOLVE_COMMAND:q"` + "\n"}, gotStandardInput)
}

func TestResolverPOSIXRunsThroughAvailableNonPOSIXShells(t *testing.T) {
	for _, shellName := range []string{"fish", "tcsh"} {
		t.Run(shellName, func(t *testing.T) {
			shell, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("%s is unavailable", shellName)
			}
			command := renderFramedResolveCommand(
				[]string{"/usr/bin/printf", "%s\\n", "hostname build.internal"},
				"START",
				"END",
			)
			arguments, standardInput := loginShellInvocation(shell)
			process := exec.Command(arguments[0], arguments[1:]...)
			process.Stdin = bytes.NewReader(standardInput)
			process.Env = resolveEnvironment([]string{
				"HOME=" + t.TempDir(),
				"PATH=/usr/bin:/bin",
			}, command)
			output, err := process.CombinedOutput()
			require.NoError(t, err, string(output))
			framed, err := framedOutput(output, "START", "END")
			require.NoError(t, err)
			assert.Equal(t, "hostname build.internal\n", string(framed))
		})
	}
}

func TestResolverPOSIXResolvesProxyJumpInConnectionOrder(t *testing.T) {
	var mu sync.Mutex
	var commands []string
	resolver := NewResolver(ResolverOptions{
		LoginShell: func() (string, error) { return "/bin/sh", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
		Run: func(_ context.Context, _ []string, _ string, environment []string, _ []byte) ([]byte, []byte, int, error) {
			command := resolveCommandFromEnvironment(t, environment)
			mu.Lock()
			commands = append(commands, command)
			mu.Unlock()
			config := ""
			switch {
			case strings.Contains(command, "'relay.example.test'"):
				config = "user relay\nhostname relay.internal\nport 22\n"
			case strings.Contains(command, "'core.example.test'"):
				config = "user core\nhostname core.internal\nport 2222\n"
			default:
				config = "user deploy\nhostname build.internal\nport 22\n" +
					"proxyjump relay.example.test,core.example.test\n"
			}
			return framedResolverOutput("nonce", config), nil, 0, nil
		},
	})

	observation, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.NoError(t, err)
	require.Len(t, observation.route, 3)
	assert.Equal(t, "relay.example.test", observation.route[0].Target.Hostname)
	assert.Equal(t, "core.example.test", observation.route[1].Target.Hostname)
	assert.Equal(t, "build.example.test", observation.route[2].Target.Hostname)
	assert.Len(t, commands, 3)
}

func TestResolverPOSIXFailsClosedForUnreviewableRoutes(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
	}{
		{name: "proxy command", config: "user deploy\nhostname build.internal\nproxycommand ssh relay -W %h:%p\n"},
		{name: "nested jump", config: "user deploy\nhostname build.internal\nproxyjump relay.example.test\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := NewResolver(ResolverOptions{
				LoginShell: func() (string, error) { return "/bin/sh", nil },
				Nonce:      func() (string, error) { return "nonce", nil },
				Run: func(_ context.Context, _ []string, _ string, environment []string, _ []byte) ([]byte, []byte, int, error) {
					command := resolveCommandFromEnvironment(t, environment)
					if test.name == "nested jump" && strings.Contains(command, "'relay.example.test'") {
						return framedResolverOutput("nonce", "hostname relay.internal\nproxyjump edge.example.test\n"), nil, 0, nil
					}
					return framedResolverOutput("nonce", test.config), nil, 0, nil
				},
			})
			_, err := resolver.Resolve(context.Background(), ResolveRequest{
				Target: Target{Hostname: "build.example.test"},
			})
			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.SSHRouteUnreviewable))
		})
	}
}

func TestResolverPOSIXRejectsTargetBeforeInvocation(t *testing.T) {
	invoked := false
	resolver := NewResolver(ResolverOptions{
		Run: func(context.Context, []string, string, []string, []byte) ([]byte, []byte, int, error) {
			invoked = true
			return nil, nil, 0, nil
		},
	})
	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{User: "$(id)", Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHInvalidTarget))
	assert.False(t, invoked)
}

func TestResolverPOSIXPreservesCancellationAndRejectsMalformedFrames(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		resolver := NewResolver(ResolverOptions{
			LoginShell: func() (string, error) { return "/bin/sh", nil },
			Nonce:      func() (string, error) { return "nonce", nil },
			Run: func(context.Context, []string, string, []string, []byte) ([]byte, []byte, int, error) {
				return nil, nil, -1, context.Canceled
			},
		})
		_, err := resolver.Resolve(context.Background(), ResolveRequest{
			Target: Target{Hostname: "build.example.test"},
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.True(t, service.IsCode(err, service.SSHResolutionFailed))
	})

	t.Run("malformed frame", func(t *testing.T) {
		resolver := NewResolver(ResolverOptions{
			LoginShell: func() (string, error) { return "/bin/sh", nil },
			Nonce:      func() (string, error) { return "nonce", nil },
			Run: func(context.Context, []string, string, []string, []byte) ([]byte, []byte, int, error) {
				return []byte("hostname build.internal\n"), nil, 0, nil
			},
		})
		_, err := resolver.Resolve(context.Background(), ResolveRequest{
			Target: Target{Hostname: "build.example.test"},
		})
		require.Error(t, err)
		assert.True(t, service.IsCode(err, service.SSHResolutionFailed))
	})
}

func TestResolverPOSIXPreservesDeadlineCause(t *testing.T) {
	resolver := NewResolver(ResolverOptions{
		LoginShell: func() (string, error) { return "/bin/sh", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
		Run: func(context.Context, []string, string, []string, []byte) ([]byte, []byte, int, error) {
			return nil, nil, -1, context.DeadlineExceeded
		},
	})
	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

func TestResolverPOSIXReportsBoundedOutputAsStableFailure(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "ssh")
	require.NoError(t, os.WriteFile(executable, []byte(
		"#!/bin/sh\ndd if=/dev/zero bs=1048577 count=1 2>/dev/null\nsleep 30\n",
	), 0o700))
	resolver := NewResolver(ResolverOptions{
		Executable: executable,
		LoginShell: func() (string, error) { return "/bin/sh", nil },
		Nonce:      func() (string, error) { return "nonce", nil },
	})

	_, err := resolver.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHResolutionFailed))
}

func framedResolverOutput(nonce, config string) []byte {
	return []byte("KWT_SSH_CONFIG_START_" + nonce + "\n" + config +
		"KWT_SSH_CONFIG_END_" + nonce + "\n")
}

func resolveCommandFromEnvironment(t *testing.T, environment []string) string {
	t.Helper()
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == resolveCommandEnvironment {
			return value
		}
	}
	require.FailNow(t, "resolve command environment is missing")
	return ""
}
