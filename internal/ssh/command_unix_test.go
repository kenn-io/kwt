//go:build !windows

package ssh

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const interactiveClientProcessHelper = "KWT_SSH_INTERACTIVE_CLIENT_PROCESS_HELPER"
const interactiveClientProcessResult = "KWT_SSH_INTERACTIVE_CLIENT_PROCESS_RESULT"

func TestRunClientProcessKeepsInteractiveTerminalReadable(t *testing.T) {
	resultPath := t.TempDir() + "/result"
	command := exec.Command(os.Args[0], "-test.run=^TestSSHInteractiveClientProcessHelper$")
	command.Env = append(
		os.Environ(),
		interactiveClientProcessHelper+"=parent",
		interactiveClientProcessResult+"="+resultPath,
	)
	terminal, err := pty.Start(command)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = terminal.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	_, err = io.WriteString(terminal, "interactive input\n")
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("interactive SSH client did not read from its terminal")
	}
	output, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	assert.Equal(t, "interactive input\n", string(output))
}

func TestSSHInteractiveClientProcessHelper(t *testing.T) {
	mode := os.Getenv(interactiveClientProcessHelper)
	if mode == "" {
		return
	}
	if mode == "child" {
		value, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(os.Getenv(interactiveClientProcessResult), []byte(value), 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := os.Setenv(interactiveClientProcessHelper, "child"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	exitCode, _, err := RunClientProcess(
		context.Background(),
		os.Args[0],
		[]string{"-test.run=^TestSSHInteractiveClientProcessHelper$"},
		"",
		os.Environ(),
		os.Stdin,
		os.Stdout,
		os.Stderr,
	)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if exitCode != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "exit %d\n", exitCode)
		os.Exit(exitCode)
	}
	os.Exit(0)
}
