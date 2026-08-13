//go:build !windows

package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunOutputCancellationTerminatesDescendantProcessGroup(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "descendant.pid")
	script := fmt.Sprintf(
		`(exec >/dev/null 2>&1; sleep 30) & child=$!; printf '%%s' "$child" > %s; wait`,
		shellQuote(pidPath),
	)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, _, err := runOutput(
			ctx, []string{"/bin/sh", "-c", script}, filepath.Dir(pidPath), os.Environ(), nil,
		)
		result <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		encoded, err := os.ReadFile(pidPath)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(encoded)))
			require.NoError(t, err)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NotZero(t, pid, "descendant did not start")
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	cancel()
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("resolver process did not stop after cancellation")
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("resolver descendant survived cancellation")
}

func TestRunOutputResolvesExecutableWithInvocationEnvironment(t *testing.T) {
	daemonDir := t.TempDir()
	invocationDir := t.TempDir()
	const executable = "kwt-ssh-path-test"
	require.NoError(t, os.WriteFile(
		filepath.Join(daemonDir, executable),
		[]byte("#!/bin/sh\nprintf daemon"),
		0o700,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(invocationDir, executable),
		[]byte("#!/bin/sh\nprintf invocation"),
		0o700,
	))
	t.Setenv("PATH", daemonDir)

	stdout, _, _, err := runOutput(
		context.Background(),
		[]string{executable},
		t.TempDir(),
		[]string{"PATH=" + invocationDir},
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "invocation", string(stdout))
}

func TestRunOutputUsesInvocationWorkingDirectory(t *testing.T) {
	workingDirectory := t.TempDir()
	stdout, _, _, err := runOutput(
		context.Background(),
		[]string{"/bin/pwd"},
		workingDirectory,
		[]string{"PATH=/usr/bin:/bin"},
		nil,
	)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(workingDirectory)
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(stdout)))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestRunOutputBoundsOutputAndCancelsResolverProcess(t *testing.T) {
	for _, test := range []struct {
		name   string
		script string
		stream func([]byte, []byte) []byte
	}{
		{
			name:   "stdout",
			script: "dd if=/dev/zero bs=1048577 count=1 2>/dev/null; sleep 30",
			stream: func(stdout, _ []byte) []byte { return stdout },
		},
		{
			name:   "stderr",
			script: "dd if=/dev/zero bs=1048577 count=1 2>/dev/null | cat >&2; sleep 30",
			stream: func(_, stderr []byte) []byte { return stderr },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started := time.Now()
			stdout, stderr, _, err := runOutput(
				ctx,
				[]string{"/bin/sh", "-c", test.script},
				t.TempDir(),
				os.Environ(),
				nil,
			)
			require.Error(t, err)
			assert.LessOrEqual(t, len(test.stream(stdout, stderr)), 1<<20)
			assert.Less(t, time.Since(started), 2*time.Second)
		})
	}
}

func TestAccountLoginShellHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := accountLoginShell(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}
