//go:build !windows

package ssh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
