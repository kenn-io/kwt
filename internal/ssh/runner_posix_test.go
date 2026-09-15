//go:build !windows

package ssh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
	"go.kenn.io/kwt/service"
)

func TestRunnerPresentsBrowserAuthenticationBeforeSSHExits(t *testing.T) {
	directory := t.TempDir()
	approved := filepath.Join(directory, "approved")
	require.NoError(t, os.WriteFile(filepath.Join(directory, "ssh"), []byte(
		"#!/bin/sh\nprintf '%s\\n' '# Tailscale SSH requires an additional check.' '# To authenticate, visit: https://login.tailscale.com/a/example' >&2\n"+
			"while [ ! -f "+shellQuote(approved)+" ]; do /bin/sleep 0.01; done\n",
	), 0o700))
	var received service.OperationPrompt
	request := LeaseRequest{
		WorkingDirectory: directory,
		Environment:      []string{"PATH=" + directory + ":/usr/bin:/bin"},
		Prompt: func(_ context.Context, prompt service.OperationPrompt) (string, error) {
			received = prompt
			return "", os.WriteFile(approved, nil, 0o600)
		},
	}
	target := ResolvedTarget{
		LogicalTarget:   Target{Hostname: "build.example.test"},
		EffectiveTarget: Target{Hostname: "build.example.test"},
		DisplayTarget:   "build.example.test",
		Projection:      ExecutionProjection{Arguments: []string{"-F", os.DevNull}},
	}
	runner, err := newRunner(filepath.Join(directory, "private"), request, target,
		runnerOptions{Version: supportedAskpassVersion()})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	status, err := runner(ctx, []string{"--", "build.example.test"})
	require.NoError(t, err)
	require.Equal(t, 0, status)
	assert.Equal(t, "ssh_browser_authentication", received.Kind)
	assert.Equal(t, "https://login.tailscale.com/a/example", received.Details["authentication_url"])
	assert.Equal(t, "browser", received.Details["method"])
	assert.Equal(t, "build.example.test", received.Details["display_target"])
	assert.NotNil(t, received.Deadline)
	assert.False(t, received.Sensitive)
}

func TestSSHRunnerUsesAccountLoginShellAndInvocationDirectory(t *testing.T) {
	workingDirectory := t.TempDir()
	bin := t.TempDir()
	executable := filepath.Join(bin, "ssh")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755))
	var capturedArguments []string
	var capturedDirectory string
	var capturedEnvironment []string

	exitCode, err := runSSHProcessWith(
		context.Background(),
		[]string{"-MNf", "--", "deploy@build.internal"},
		workingDirectory,
		[]string{"PATH=" + bin, "SSH_AUTH_SOCK=/tmp/agent"},
		func(
			_ context.Context,
			arguments []string,
			directory string,
			environment []string,
			_ []byte,
		) ([]byte, []byte, int, error) {
			capturedArguments = append([]string(nil), arguments...)
			capturedDirectory = directory
			capturedEnvironment = append([]string(nil), environment...)
			return nil, nil, 0, nil
		},
		func(context.Context) (string, error) { return "/bin/zsh", nil },
	)

	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, []string{
		"/bin/zsh", "-l", "-c", `exec /bin/sh -c "$KWT_SSH_EXEC_COMMAND"`,
	}, capturedArguments)
	assert.Equal(t, workingDirectory, capturedDirectory)
	command := ""
	for _, value := range capturedEnvironment {
		if strings.HasPrefix(value, "KWT_SSH_EXEC_COMMAND=") {
			command = strings.TrimPrefix(value, "KWT_SSH_EXEC_COMMAND=")
		}
	}
	assert.Contains(t, command, "cd "+shellQuote(workingDirectory))
	assert.Contains(t, command, "exec "+shellQuote(executable))
	assert.Contains(t, command, shellQuote("deploy@build.internal"))
}

func TestSSHRunnerPreservesProcessDiagnosticForClients(t *testing.T) {
	directory := t.TempDir()
	diagnostic := "deploy@build.example.test: Permission denied (publickey)."
	require.NoError(t, os.WriteFile(filepath.Join(directory, "ssh"), []byte(
		"#!/bin/sh\nprintf '%s\\n' '"+diagnostic+"' >&2\nexit 255\n",
	), 0o700))

	status, err := runSSHProcessWith(context.Background(), nil, directory,
		[]string{"PATH=" + directory}, runOutput,
		func(context.Context) (string, error) { return "/bin/sh", nil },
	)
	require.Equal(t, 255, status)
	require.Error(t, err)
	failure := service.AsError(mapManagerError(&openssh.CommandError{
		Operation: "master start", ExitCode: status, Err: err,
	}))
	assert.Equal(t, service.SSHConnectionFailed, failure.Code)
	assert.Contains(t, failure.Message, diagnostic)
	assert.Equal(t, 255, failure.Details["exit_code"])
}
