package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	managed "go.kenn.io/kit/git/managed"
	shared "go.kenn.io/kwt/worktree"
)

func TestSharedWorktreeExecutionPreservesCallerPolicy(t *testing.T) {
	repo := NewTestRepository(t)
	config := filepath.Join(t.TempDir(), "global.gitconfig")
	require.NoError(t, os.WriteFile(config, []byte("[fixture]\n value = inherited\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("KWT_FIXTURE_SECRET", "fixture-value")
	for _, strip := range []bool{false, true} {
		names := []string(nil)
		want := "fixture-value"
		if strip {
			names = []string{"KWT_FIXTURE_SECRET"}
			want = ""
		}
		opts, err := New(repo.Path).WorktreeExecution(names, nil)
		require.NoError(t, err)
		lifecycle, err := OpenWorktrees(t.Context(), opts)
		require.NoError(t, err)
		require.NoError(t, lifecycle.WithLock(t.Context(), func(s *shared.Scope) error {
			out, err := s.RunGit(t.Context(), repo.Path, "config", "--get", "fixture.value")
			require.NoError(t, err)
			require.Equal(t, "inherited\n", string(out))
			out, err = s.RunGit(t.Context(), repo.Path, "-c", "alias.probe=!printf '%s' \"$KWT_FIXTURE_SECRET\"", "probe")
			require.NoError(t, err)
			require.Equal(t, want, string(out))
			return nil
		}))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opts, err := NewWithContext(ctx, repo.Path).WorktreeExecution(nil, nil)
	require.NoError(t, err)
	_, err = OpenWorktrees(ctx, opts)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSharedWorktreeCreationAcceptsSuccessfulRetainedHookPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX checkout hook")
	}
	repo := NewTestRepository(t)
	pidPath := filepath.Join(t.TempDir(), "hook.pid")
	t.Setenv("KWT_FIXTURE_HOOK_PID", pidPath)
	t.Cleanup(func() {
		data, err := os.ReadFile(pidPath)
		if os.IsNotExist(err) {
			return
		}
		require.NoError(t, err)
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		require.NoError(t, err)
		process, err := os.FindProcess(pid)
		require.NoError(t, err)
		_ = process.Kill()
	})
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\nsleep 30 &\nprintf '%s' \"$!\" > \"$KWT_FIXTURE_HOOK_PID\"\n"), 0o700))
	opts, err := New(repo.Path).WorktreeExecution(nil, nil)
	require.NoError(t, err)
	lifecycle, err := OpenWorktrees(t.Context(), opts)
	require.NoError(t, err)
	started := time.Now()
	result, err := lifecycle.Create(t.Context(), shared.CreateRequest{Git: managed.CreateWorktreeOptions{Path: filepath.Join(t.TempDir(), "created"), Branch: "created", Mode: managed.CheckoutNewBranch}, Serialization: shared.CreateHookReentrant, Identity: shared.IdentityPolicy{FileName: "kwt-generation", Generate: true}})
	require.NoError(t, err)
	require.NotEmpty(t, result.IdentityValue)
	require.Less(t, time.Since(started), time.Second)
	require.FileExists(t, pidPath)
}
