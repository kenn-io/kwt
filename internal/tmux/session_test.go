package tmux

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/url"
)

func TestDefaultSessionConfig(t *testing.T) {
	config := DefaultSessionConfig()

	if !config.Enabled {
		t.Error("Expected Enabled to be true")
	}

	if config.TmuxCommand != "tmux" {
		t.Errorf("Expected TmuxCommand to be 'tmux', got '%s'", config.TmuxCommand)
	}

	if config.HistoryLimit != 50000 {
		t.Errorf("Expected HistoryLimit to be 50000, got %d", config.HistoryLimit)
	}
}

func TestSessionOptionsCreation(t *testing.T) {
	opts := SessionOptions{
		Context:    "test",
		Identifier: "test-session",
		WorkingDir: "/tmp",
		Command:    "echo hello",
		Metadata: map[string]string{
			"created_by": "test",
		},
	}

	if opts.Context != "test" {
		t.Errorf("Expected Context to be 'test', got '%s'", opts.Context)
	}

	if opts.Identifier != "test-session" {
		t.Errorf("Expected Identifier to be 'test-session', got '%s'", opts.Identifier)
	}

	if opts.Command != "echo hello" {
		t.Errorf("Expected Command to be 'echo hello', got '%s'", opts.Command)
	}
}

func TestWorkspaceSessionName(t *testing.T) {
	info := &url.RepositoryInfo{
		Host: "github.com", Owner: "wesm", Repository: "kwt",
		FullPath: "github.com/wesm/kwt",
	}
	worktreePath := "/home/u/worktrees/github.com/wesm/kwt/feature/foo"
	name := WorkspaceSessionName(info, "feature/foo", worktreePath)

	// Golden value pins field order, the worktreePath hash basis, and
	// sanitization at once — a swapped/dropped field or a narrowed hash input
	// trips this where the structural checks below would not.
	assert.Equal(t, "kwt-wt-kwt-feature-foo-9cc4e551", name)

	assert.True(t, strings.HasPrefix(name, "kwt-"), "must carry the kwt- prefix")
	assert.NotContains(t, name, ".", "tmux names cannot contain '.'")
	assert.NotContains(t, name, ":", "tmux names cannot contain ':'")
	assert.NotContains(t, name, "/", "slashes must be sanitized")
	assert.NotContains(t, name, "github-com")
	assert.NotContains(t, name, "wesm")

	// Stable across calls for the same (info, branch, worktreePath).
	assert.Equal(t, name, WorkspaceSessionName(info, "feature/foo", worktreePath))

	// Two detached-HEAD worktrees of the same repo share info and branch
	// "HEAD" (git reports "HEAD" for any detached worktree), but have distinct
	// worktree paths — they must not collide on the same session name.
	detached1 := WorkspaceSessionName(info, "HEAD", "/home/u/worktrees/github.com/wesm/kwt/wt1")
	detached2 := WorkspaceSessionName(info, "HEAD", "/home/u/worktrees/github.com/wesm/kwt/wt2")
	assert.NotEqual(t, detached1, detached2)
}

func TestWorkspaceSessionNameDoesNotOverlapDirectoryWorkspaceNamespace(t *testing.T) {
	info := &url.RepositoryInfo{Repository: "workspace"}
	path := "/worktrees/notes"

	assert.NotEqual(
		t,
		DirWorkspaceSessionName("notes", path),
		WorkspaceSessionName(info, "dir-notes", path),
	)
}

func TestMatchesWorkspaceSessionName(t *testing.T) {
	info := &url.RepositoryInfo{
		Host: "github.com", Owner: "wesm", Repository: "kwt",
		FullPath: "github.com/wesm/kwt",
	}
	branch := "feature/foo"
	path := "/home/u/worktrees/github.com/wesm/kwt/feature/foo"

	assert.True(t, MatchesWorkspaceSessionName(
		"kwt-wt-kwt-feature-foo-9cc4e551", info, branch, path,
	))
	assert.True(t, MatchesWorkspaceSessionName(
		"kwt-workspace-github-com-wesm-kwt-feature-foo-9cc4e551",
		info,
		branch,
		path,
	))
	assert.False(t, MatchesWorkspaceSessionName(
		"arbitrary-session", info, branch, path,
	))
	assert.False(t, MatchesWorkspaceSessionName(
		"kwt-wt-kwt-feature-foo-9cc4e551", info, branch, "/another/path",
	))
}

func TestMatchesLegacyWorkspaceSessionPathAfterBranchChange(t *testing.T) {
	path := "/home/u/worktrees/github.com/wesm/kwt/feature/foo"

	assert.True(t, MatchesLegacyWorkspaceSessionPath(
		"kwt-wt-kwt-old-branch-9cc4e551",
		path,
	))
	assert.True(t, MatchesLegacyWorkspaceSessionPath(
		"kwt-workspace-github-com-wesm-kwt-old-branch-9cc4e551",
		path,
	))
	assert.False(t, MatchesLegacyWorkspaceSessionPath(
		"unrelated-old-branch-9cc4e551",
		path,
	))
	assert.False(t, MatchesLegacyWorkspaceSessionPath(
		"kwt-wt-kwt-old-branch-9cc4e551",
		"/another/path",
	))
}

func TestWorktreeSessionNamespaceTreatsLegacyDirPrefixAsAmbiguous(t *testing.T) {
	assert.True(t, IsKWTWorktreeSessionName("kwt-wt-repo-topic-deadbeef"))
	assert.True(t, IsKWTWorktreeSessionName("kwt-workspace-host-owner-repo-topic-deadbeef"))
	assert.True(t, IsKWTWorktreeSessionName("kwt-workspace-dir-notes-deadbeef"))
}

func TestDirWorkspaceSessionName(t *testing.T) {
	name := DirWorkspaceSessionName("my notes", "/Users/me/notes")

	assert.True(t, strings.HasPrefix(name, "kwt-workspace-dir-my-notes-"), name)
	assert.Regexp(t, `^kwt-workspace-dir-my-notes-[0-9a-f]{8}$`, name)
	assert.Equal(t, name, DirWorkspaceSessionName("my notes", "/Users/me/notes"),
		"session names must be deterministic")
	assert.NotEqual(t, name, DirWorkspaceSessionName("my notes", "/Users/me/other"),
		"different paths must not collide")
}

func TestMatchDirWorkspaceSessionMatchesByPathHash(t *testing.T) {
	current := DirWorkspaceSessionName("new-name", "/Users/me/notes")
	old := DirWorkspaceSessionName("old-name", "/Users/me/notes")
	sessions := []string{"kwt-workspace-foo-12345678", old, "unrelated"}

	got, ok := MatchDirWorkspaceSession(sessions, "/Users/me/notes")

	require.True(t, ok)
	assert.Equal(t, old, got, "renamed workspaces must re-attach to the live old-name session")
	assert.NotEqual(t, current, got)

	_, ok = MatchDirWorkspaceSession([]string{"kwt-workspace-foo-12345678"}, "/Users/me/notes")
	assert.False(t, ok)
}
