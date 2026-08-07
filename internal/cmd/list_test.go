package cmd

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/ui"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

func TestImportedWorktreeReceivesProtectedSocketIdentity(t *testing.T) {
	worktrees := []models.Worktree{{
		Path:        "/worktrees/pr-32",
		Branch:      "pr-32",
		Repository:  "github.com/acme/widget",
		SessionName: "kwt-workspace-pr-32",
	}}
	annotateProtectedSocketIdentity(worktrees, map[string]pullrequest.Provenance{
		"pr-32": {
			Project: pullrequest.Project{
				Identity: "github.com/acme/widget",
			},
			Workspace: pullrequest.Workspace{
				Path:        "/worktrees/pr-32",
				Branch:      "pr-32",
				Repository:  "github.com/acme/widget",
				SessionName: "kwt-workspace-pr-32",
			},
		},
	})

	want := tmux.ProtectedWorkspaceSocketName(
		"kwt-workspace-pr-32",
		"/worktrees/pr-32",
	)
	if worktrees[0].TmuxSocketName != want {
		t.Fatalf("tmux socket = %q, want %q", worktrees[0].TmuxSocketName, want)
	}
}

func TestStaleProvenanceDoesNotLabelReusedWorktreePath(t *testing.T) {
	record := pullrequest.Provenance{
		Project: pullrequest.Project{Identity: "github.com/acme/widget"},
		Workspace: pullrequest.Workspace{
			Path:        "/worktrees/reused",
			Branch:      "pr-32",
			Repository:  "github.com/acme/widget",
			SessionName: "kwt-workspace-pr-32",
		},
	}
	tests := []models.Worktree{
		{
			Path: "/worktrees/reused", Branch: "other",
			Repository:  "github.com/acme/widget",
			SessionName: "kwt-workspace-pr-32",
		},
		{
			Path: "/worktrees/reused", Branch: "pr-32",
			Repository:  "github.com/acme/other",
			SessionName: "kwt-workspace-pr-32",
		},
		{
			Path: "/worktrees/reused", Branch: "pr-32",
			Repository:  "github.com/acme/widget",
			SessionName: "kwt-workspace-other",
		},
	}

	annotateProtectedSocketIdentity(tests, map[string]pullrequest.Provenance{
		"stale": record,
	})

	for _, worktree := range tests {
		assert.Empty(t, worktree.TmuxSocketName)
	}
}

func TestStaleProvenanceGenerationDoesNotLabelReusedWorktreePath(t *testing.T) {
	worktrees := []models.Worktree{{
		Path: "/worktrees/reused", Branch: "pr-32",
		Repository: "github.com/acme/widget", SessionName: "kwt-workspace-pr-32",
		Generation: "0123456789abcdef0123456789abcdef",
	}}
	annotateProtectedSocketIdentity(worktrees, map[string]pullrequest.Provenance{
		"pr-32": {
			Project: pullrequest.Project{Identity: "github.com/acme/widget"},
			Workspace: pullrequest.Workspace{
				Path: "/worktrees/reused", Branch: "pr-32",
				Repository: "github.com/acme/widget", SessionName: "kwt-workspace-pr-32",
				Generation: "fedcba9876543210fedcba9876543210",
			},
		},
	})

	assert.Empty(t, worktrees[0].TmuxSocketName)
}

func TestProtectedSocketEnrichmentReportsUnreadableProvenance(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	require.NoError(t, os.WriteFile(
		filepath.Join(kwtHome, "pull-requests.json"),
		[]byte("{"),
		0o600,
	))

	err := enrichProtectedSocketIdentity(
		context.Background(),
		[]models.Worktree{{Path: "/worktrees/pr-32"}},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull-request provenance")
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything it wrote. The ui.Printer writes to os.Stdout directly, so this is
// how its output is observed in tests.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading pipe: %v", err)
	}
	return string(out)
}

func newGlobalListContext(baseDir string) *CommandContext {
	return &CommandContext{
		Config:  &models.Config{Worktree: models.WorktreeConfig{BaseDir: baseDir}},
		Printer: ui.New(&models.UIConfig{}),
	}
}

// TestShowGlobalWorktreesEmptyJSONEmitsArray pins that an empty global result
// in JSON mode emits a JSON array ("[]"), not the human-readable prose, so
// scripts can parse the output unconditionally.
func TestShowGlobalWorktreesEmptyJSONEmitsArray(t *testing.T) {
	ctx := newGlobalListContext(t.TempDir())

	listJSON = true
	defer func() { listJSON = false }()

	out := captureStdout(t, func() {
		if err := showGlobalWorktrees(ctx); err != nil {
			t.Fatalf("showGlobalWorktrees: %v", err)
		}
	})

	if out != "[]\n" {
		t.Errorf("empty -g --json output = %q, want %q", out, "[]\n")
	}
}

// TestShowGlobalWorktreesEmptyProsePrintsMessage pins that non-JSON mode still
// prints the human-readable "No worktrees found" message.
func TestShowGlobalWorktreesEmptyProsePrintsMessage(t *testing.T) {
	baseDir := t.TempDir()
	ctx := newGlobalListContext(baseDir)

	listJSON = false

	out := captureStdout(t, func() {
		if err := showGlobalWorktrees(ctx); err != nil {
			t.Fatalf("showGlobalWorktrees: %v", err)
		}
	})

	if !strings.Contains(out, "No worktrees found in "+baseDir) {
		t.Errorf("empty -g prose output = %q, want it to mention the base dir", out)
	}
	if strings.Contains(out, "[]") {
		t.Errorf("prose mode must not emit a JSON array; got %q", out)
	}
}

func TestEnrichManifestFields(t *testing.T) {
	t.Run("happy path - populates Repository from git origin", func(t *testing.T) {
		// Create a temporary directory for our test git repo
		tempDir := t.TempDir()

		// Initialize git repo
		cmd := exec.Command("git", "init")
		cmd.Dir = tempDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to init git repo: %v", err)
		}

		// Add origin remote
		cmd = exec.Command("git", "remote", "add", "origin", "https://github.com/example/demo.git")
		cmd.Dir = tempDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to add origin remote: %v", err)
		}

		// Create git instance pointing to our temp repo
		g := git.New(tempDir)

		// Create CommandContext with the git instance
		ctx := &CommandContext{
			Git:    g,
			Config: &models.Config{},
		}

		// Create a worktree with empty Repository field
		worktrees := []models.Worktree{
			{
				Path:       tempDir,
				Branch:     "main",
				CommitHash: "",
				IsMain:     true,
				Repository: "", // Should be populated
			},
		}

		// Call enrichWorktreeIdentity
		enrichWorktreeIdentity(ctx.Git, ctx.Config.Projects, worktrees)

		// Verify Repository is populated
		if worktrees[0].Repository == "" {
			t.Error("Repository field should be populated after enrichWorktreeIdentity")
		}

		// Verify it contains the expected path structure (github.com/example/demo)
		if worktrees[0].Repository != "github.com/example/demo" {
			t.Errorf("expected Repository to be 'github.com/example/demo', got %q", worktrees[0].Repository)
		}
		info, err := worktree.RepositoryInfoFromGit(g)
		if err != nil {
			t.Fatalf("resolve repository info: %v", err)
		}
		wantSession := tmux.WorkspaceSessionName(info, worktrees[0].Branch, worktrees[0].Path)
		if worktrees[0].SessionName != wantSession {
			t.Errorf("expected SessionName %q, got %q", wantSession, worktrees[0].SessionName)
		}
	})

	t.Run("registered identity wins over fork origin", func(t *testing.T) {
		tempDir := t.TempDir()

		cmd := exec.Command("git", "init")
		cmd.Dir = tempDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to init git repo: %v", err)
		}
		cmd = exec.Command("git", "remote", "add", "origin", "https://github.com/fork/demo.git")
		cmd.Dir = tempDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to add origin remote: %v", err)
		}

		g := git.New(tempDir)
		ctx := &CommandContext{
			Git: g,
			Config: &models.Config{
				Projects: []models.Project{
					{Repository: "github.com/upstream/demo", Path: tempDir},
				},
			},
		}
		worktrees := []models.Worktree{
			{Path: tempDir, Branch: "main", IsMain: true},
		}

		enrichWorktreeIdentity(ctx.Git, ctx.Config.Projects, worktrees)

		if worktrees[0].Repository != "github.com/upstream/demo" {
			t.Errorf("Repository = %q, want registered identity github.com/upstream/demo",
				worktrees[0].Repository)
		}
	})

	t.Run("fallback path - non-git directory leaves Repository empty", func(t *testing.T) {
		// Create a temporary directory that is NOT a git repo
		tempDir := t.TempDir()

		// Create git instance pointing to non-git directory
		g := git.New(tempDir)

		// Create CommandContext with the git instance
		ctx := &CommandContext{
			Git:    g,
			Config: &models.Config{},
		}

		// Create a worktree with empty Repository field
		worktrees := []models.Worktree{
			{
				Path:       tempDir,
				Branch:     "main",
				CommitHash: "",
				IsMain:     true,
				Repository: "", // Should remain empty
			},
		}

		// Call enrichWorktreeIdentity - should not error
		enrichWorktreeIdentity(ctx.Git, ctx.Config.Projects, worktrees)

		// Verify Repository remains empty (best-effort, no error)
		if worktrees[0].Repository != "" {
			t.Errorf("expected Repository to remain empty for non-git directory, got %q", worktrees[0].Repository)
		}
	})

	t.Run("fallback path - git repo without origin populates Repository from local path", func(t *testing.T) {
		// Create a temporary directory for our test git repo
		tempDir := t.TempDir()

		// Initialize git repo WITHOUT adding origin
		cmd := exec.Command("git", "init")
		cmd.Dir = tempDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to init git repo: %v", err)
		}

		// Create git instance pointing to our temp repo (no origin)
		g := git.New(tempDir)

		// Create CommandContext with the git instance
		ctx := &CommandContext{
			Git:    g,
			Config: &models.Config{},
		}

		// Create a worktree with empty Repository field
		worktrees := []models.Worktree{
			{
				Path:       tempDir,
				Branch:     "main",
				CommitHash: "",
				IsMain:     true,
				Repository: "", // Will fall back to local path
			},
		}

		// Call enrichWorktreeIdentity
		enrichWorktreeIdentity(ctx.Git, ctx.Config.Projects, worktrees)

		// Verify Repository is populated with local fallback (directory name based)
		// The fallback uses the directory name or local path, so it should not be empty
		if worktrees[0].Repository == "" {
			t.Error("Repository field should be populated with local fallback")
		}
	})

	t.Run("idempotent - calling multiple times doesn't break anything", func(t *testing.T) {
		tempDir := t.TempDir()

		// Initialize git repo
		cmd := exec.Command("git", "init")
		cmd.Dir = tempDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to init git repo: %v", err)
		}

		// Add origin remote
		cmd = exec.Command("git", "remote", "add", "origin", "https://github.com/example/test.git")
		cmd.Dir = tempDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to add origin remote: %v", err)
		}

		g := git.New(tempDir)
		ctx := &CommandContext{
			Git:    g,
			Config: &models.Config{},
		}

		worktrees := []models.Worktree{
			{
				Path:       tempDir,
				Branch:     "main",
				CommitHash: "",
				IsMain:     true,
				Repository: "",
			},
		}

		// Call twice
		enrichWorktreeIdentity(ctx.Git, ctx.Config.Projects, worktrees)
		firstResult := worktrees[0].Repository
		enrichWorktreeIdentity(ctx.Git, ctx.Config.Projects, worktrees)
		secondResult := worktrees[0].Repository

		// Should be the same both times
		if firstResult != secondResult {
			t.Errorf("idempotent call produced different results: %q vs %q", firstResult, secondResult)
		}
	})
}
