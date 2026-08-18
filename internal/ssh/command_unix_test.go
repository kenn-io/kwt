//go:build !windows

package ssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const interactiveClientProcessHelper = "KWT_SSH_INTERACTIVE_CLIENT_PROCESS_HELPER"
const interactiveClientProcessResult = "KWT_SSH_INTERACTIVE_CLIENT_PROCESS_RESULT"

func TestRunClientProcessPreservesInteractiveTerminalJobControl(t *testing.T) {
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
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(resultPath + ".ready")
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	parentGroup, err := os.ReadFile(resultPath + ".parent")
	require.NoError(t, err)
	childGroup, err := os.ReadFile(resultPath + ".ready")
	require.NoError(t, err)
	require.NotEqual(t, string(parentGroup), string(childGroup))
	childGroupID, err := strconv.Atoi(string(childGroup))
	require.NoError(t, err)
	require.NoError(t, syscall.Kill(-childGroupID, syscall.SIGTSTP))
	var status syscall.WaitStatus
	_, err = syscall.Wait4(command.Process.Pid, &status, syscall.WUNTRACED, nil)
	require.NoError(t, err)
	require.True(t, status.Stopped() || status.Continued())
	require.NoError(t, syscall.Kill(-command.Process.Pid, syscall.SIGCONT))
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

func TestRunClientProcessCancellationTerminatesInteractiveDescendants(t *testing.T) {
	resultPath := t.TempDir() + "/cancel"
	command := exec.Command(os.Args[0], "-test.run=^TestSSHInteractiveClientProcessHelper$")
	command.Env = append(
		os.Environ(),
		interactiveClientProcessHelper+"=cancel-parent",
		interactiveClientProcessResult+"="+resultPath,
	)
	terminal, err := pty.Start(command)
	require.NoError(t, err)
	var terminalOutput bytes.Buffer
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(&terminalOutput, terminal)
		close(drained)
	}()
	t.Cleanup(func() {
		_ = terminal.Close()
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
	})
	waitErr := command.Wait()
	require.NoError(t, terminal.Close())
	<-drained
	require.NoError(t, waitErr, terminalOutput.String())

	encoded, err := os.ReadFile(resultPath + ".descendant")
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(encoded))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, time.Second, 10*time.Millisecond, "interactive descendant survived cancellation")
}

func TestSSHInteractiveClientProcessHelper(t *testing.T) {
	mode := os.Getenv(interactiveClientProcessHelper)
	if mode == "" {
		return
	}
	if mode == "child" {
		group := []byte(strconv.Itoa(syscall.Getpgrp()))
		if err := os.WriteFile(os.Getenv(interactiveClientProcessResult)+".ready", group, 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
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
	if mode == "cancel-parent" {
		pidPath := os.Getenv(interactiveClientProcessResult) + ".descendant"
		script := fmt.Sprintf(
			`sleep 30 & child=$!; printf '%%s' "$child" > %s; wait`,
			shellQuote(pidPath),
		)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			for {
				if _, err := os.Stat(pidPath); err == nil {
					cancel()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		_, _, err := RunClientProcess(
			ctx,
			"/bin/sh",
			[]string{"-c", script},
			"",
			os.Environ(),
			os.Stdin,
			os.Stdout,
			os.Stderr,
		)
		if !errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := os.Setenv(interactiveClientProcessHelper, "child"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	group := []byte(strconv.Itoa(syscall.Getpgrp()))
	if err := os.WriteFile(os.Getenv(interactiveClientProcessResult)+".parent", group, 0o600); err != nil {
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
