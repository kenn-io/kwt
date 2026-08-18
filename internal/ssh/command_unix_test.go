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
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const interactiveClientProcessHelper = "KWT_SSH_INTERACTIVE_CLIENT_PROCESS_HELPER"
const interactiveClientProcessResult = "KWT_SSH_INTERACTIVE_CLIENT_PROCESS_RESULT"
const interactiveClientProcessBackgroundStart = "KWT_SSH_INTERACTIVE_CLIENT_PROCESS_BACKGROUND_START"

func TestClientWaitErrorPreservesSignalExitStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal syscall.Signal
		want   int
	}{
		{name: "interrupt", signal: syscall.SIGINT, want: 130},
		{name: "terminate", signal: syscall.SIGTERM, want: 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := clientWaitError{status: syscall.WaitStatus(test.signal)}
			assert.Equal(t, test.want, err.ExitCode())
		})
	}
}

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
	parentGroupID := waitForProcessGroupFile(t, resultPath+".parent")
	childGroupID := waitForProcessGroupFile(t, resultPath+".ready")
	require.NotEqual(t, parentGroupID, childGroupID)
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

func TestRunClientProcessSuspendsPipelineSiblings(t *testing.T) {
	resultPath := t.TempDir() + "/pipeline"
	command := exec.Command(os.Args[0], "-test.run=^TestSSHInteractiveClientProcessHelper$")
	command.Env = append(
		os.Environ(),
		interactiveClientProcessHelper+"=pipeline-parent",
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
	parentGroupID := waitForProcessGroupFile(t, resultPath+".parent")
	siblingGroupID := waitForProcessGroupFile(t, resultPath+".sibling-group")
	require.Equal(t, parentGroupID, siblingGroupID)
	childGroupID := waitForProcessGroupFile(t, resultPath+".ready")
	require.NoError(t, syscall.Kill(-childGroupID, syscall.SIGTSTP))
	var status syscall.WaitStatus
	_, err = syscall.Wait4(command.Process.Pid, &status, syscall.WUNTRACED, nil)
	require.NoError(t, err)
	require.True(t, status.Stopped() || status.Continued())
	require.NoError(t, os.WriteFile(resultPath+".sibling-probe", nil, 0o600))
	require.Never(t, func() bool {
		_, statErr := os.Stat(resultPath + ".sibling-observed")
		return statErr == nil
	}, 250*time.Millisecond, 10*time.Millisecond)
	require.NoError(t, syscall.Kill(-parentGroupID, syscall.SIGCONT))
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(resultPath + ".sibling-observed")
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	_, err = io.WriteString(terminal, "pipeline input\n")
	require.NoError(t, err)
	require.NoError(t, command.Wait())
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

func TestRunClientProcessForegroundsTerminalOutputWithNonterminalInput(t *testing.T) {
	testRunClientProcessTerminalOutputWithNonterminalInput(t, false, false)
}

func TestRunClientProcessDoesNotStealTerminalAfterBackgroundResume(t *testing.T) {
	testRunClientProcessTerminalOutputWithNonterminalInput(t, false, true)
}

func TestRunClientProcessDoesNotStealTerminalWhenStartedInBackground(t *testing.T) {
	testRunClientProcessTerminalOutputWithNonterminalInput(t, true, false)
}

func TestRunClientCommandRestoresTerminalAfterStartFailure(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestSSHInteractiveClientProcessHelper$")
	command.Env = append(
		os.Environ(),
		interactiveClientProcessHelper+"=start-failure",
	)
	terminal, err := pty.Start(command)
	require.NoError(t, err)
	t.Cleanup(func() { _ = terminal.Close() })
	require.NoError(t, command.Wait())
}

func testRunClientProcessTerminalOutputWithNonterminalInput(
	t *testing.T,
	backgroundStart bool,
	backgroundResume bool,
) {
	t.Helper()
	resultPath := t.TempDir() + "/nonterminal"
	command := exec.Command(os.Args[0], "-test.run=^TestSSHInteractiveClientProcessHelper$")
	command.Env = append(
		os.Environ(),
		interactiveClientProcessHelper+"=nonterminal-parent",
		interactiveClientProcessResult+"="+resultPath,
	)
	if backgroundStart {
		command.Env = append(command.Env, interactiveClientProcessBackgroundStart+"=1")
	}
	terminal, err := pty.Start(command)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = terminal.Close()
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
	})
	shellGroupID := waitForProcessGroupFile(t, resultPath+".shell")
	t.Cleanup(func() { _ = syscall.Kill(-shellGroupID, syscall.SIGKILL) })
	if backgroundStart {
		require.NoError(t, os.WriteFile(resultPath+".background", nil, 0o600))
		require.Eventually(t, func() bool {
			_, readyErr := os.Stat(resultPath + ".background-ready")
			return readyErr == nil
		}, 5*time.Second, 10*time.Millisecond)
	}
	childGroupID := waitForProcessGroupFile(t, resultPath+".ready")
	foregroundGroup, err := terminalForegroundProcessGroup(terminal)
	require.NoError(t, err)
	if backgroundStart {
		assert.Equal(t, shellGroupID, foregroundGroup)
		require.NoError(t, os.WriteFile(resultPath+".continue", nil, 0o600))
		require.NoError(t, command.Wait())
	} else {
		assert.Equal(t, childGroupID, foregroundGroup)
		require.NoError(t, syscall.Kill(-childGroupID, syscall.SIGTSTP))
		var status syscall.WaitStatus
		_, err = syscall.Wait4(command.Process.Pid, &status, syscall.WUNTRACED, nil)
		require.NoError(t, err)
		require.True(t, status.Stopped() || status.Continued())
		if backgroundResume {
			require.NoError(t, os.WriteFile(resultPath+".background", nil, 0o600))
			require.Eventually(t, func() bool {
				_, readyErr := os.Stat(resultPath + ".background-ready")
				return readyErr == nil
			}, 5*time.Second, 10*time.Millisecond)
		}
		require.NoError(t, syscall.Kill(-command.Process.Pid, syscall.SIGCONT))
		require.NoError(t, os.WriteFile(resultPath+".continue", nil, 0o600))
		require.NoError(t, command.Wait())
	}
	if backgroundStart || backgroundResume {
		encodedForeground, readErr := os.ReadFile(resultPath + ".final-foreground")
		require.NoError(t, readErr)
		foregroundGroup, err = strconv.Atoi(string(encodedForeground))
		require.NoError(t, err)
		assert.Equal(t, shellGroupID, foregroundGroup)
	}
}

func waitForProcessGroupFile(t *testing.T, path string) int {
	t.Helper()
	var processGroup int
	require.Eventually(t, func() bool {
		encoded, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		processGroup, err = strconv.Atoi(string(encoded))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	return processGroup
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
	if mode == "start-failure" {
		before, err := terminalForegroundProcessGroup(os.Stdout)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		command := exec.Command("/path/that/does/not/exist")
		command.Stdin = os.Stdin
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		started, startErr := runClientCommand(context.Background(), command)
		if started || startErr == nil {
			_, _ = fmt.Fprintln(os.Stderr, "invalid start result")
			os.Exit(1)
		}
		after, err := terminalForegroundProcessGroup(os.Stdout)
		if err != nil || after != before {
			_, _ = fmt.Fprintf(os.Stderr, "terminal changed from %d to %d: %v\n", before, after, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode == "nonterminal-child" {
		group := []byte(strconv.Itoa(syscall.Getpgrp()))
		resultPath := os.Getenv(interactiveClientProcessResult)
		if err := os.WriteFile(resultPath+".ready", group, 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for {
			if _, err := os.Stat(resultPath + ".continue"); err == nil {
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if mode == "shell" {
		signal.Ignore(syscall.SIGTTOU)
		resultPath := os.Getenv(interactiveClientProcessResult)
		for {
			if _, err := os.Stat(resultPath + ".background"); err == nil {
				if err := setTerminalForeground(os.Stdout, syscall.Getpgrp()); err != nil {
					_, _ = fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				if err := os.WriteFile(resultPath+".background-ready", nil, 0o600); err != nil {
					_, _ = fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				select {}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if mode == "nonterminal-parent" {
		shell := exec.Command(os.Args[0], "-test.run=^TestSSHInteractiveClientProcessHelper$")
		shell.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		shell.Env = append(
			os.Environ(),
			interactiveClientProcessHelper+"=shell",
			interactiveClientProcessResult+"="+os.Getenv(interactiveClientProcessResult),
		)
		shell.Stdin = os.Stdin
		shell.Stdout = os.Stdout
		shell.Stderr = os.Stderr
		if err := shell.Start(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(
			os.Getenv(interactiveClientProcessResult)+".shell",
			[]byte(strconv.Itoa(shell.Process.Pid)),
			0o600,
		); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if os.Getenv(interactiveClientProcessBackgroundStart) == "1" {
			for {
				if _, err := os.Stat(
					os.Getenv(interactiveClientProcessResult) + ".background-ready",
				); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		if err := os.Setenv(interactiveClientProcessHelper, "nonterminal-child"); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		exitCode, _, err := RunClientProcess(
			context.Background(),
			os.Args[0],
			[]string{"-test.run=^TestSSHInteractiveClientProcessHelper$"},
			"",
			os.Environ(),
			strings.NewReader("batch input"),
			os.Stdout,
			os.Stderr,
		)
		if err != nil || exitCode != 0 {
			_, _ = fmt.Fprintf(os.Stderr, "exit %d: %v\n", exitCode, err)
			os.Exit(1)
		}
		foregroundGroup, err := terminalForegroundProcessGroup(os.Stdout)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(
			os.Getenv(interactiveClientProcessResult)+".final-foreground",
			[]byte(strconv.Itoa(foregroundGroup)),
			0o600,
		); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = syscall.Kill(-shell.Process.Pid, syscall.SIGKILL)
		_ = shell.Wait()
		os.Exit(0)
	}
	if mode == "pipeline-sibling" {
		resultPath := os.Getenv(interactiveClientProcessResult)
		if err := os.WriteFile(
			resultPath+".sibling-group",
			[]byte(strconv.Itoa(syscall.Getpgrp())),
			0o600,
		); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for {
			if _, err := os.Stat(resultPath + ".sibling-probe"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := os.WriteFile(resultPath+".sibling-observed", nil, 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		select {}
	}
	var pipelineSibling *exec.Cmd
	if mode == "pipeline-parent" {
		pipelineSibling = exec.Command(os.Args[0], "-test.run=^TestSSHInteractiveClientProcessHelper$")
		pipelineSibling.Env = append(
			os.Environ(),
			interactiveClientProcessHelper+"=pipeline-sibling",
		)
		pipelineSibling.Stdin = os.Stdin
		pipelineSibling.Stdout = os.Stdout
		pipelineSibling.Stderr = os.Stderr
		if err := pipelineSibling.Start(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
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
	if pipelineSibling != nil {
		_ = pipelineSibling.Process.Kill()
		_ = pipelineSibling.Wait()
	}
	os.Exit(0)
}
