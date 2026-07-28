package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

type recordingOpenWorkspaceRunner struct {
	ensured  bool
	attached bool
}

func (r *recordingOpenWorkspaceRunner) Ensure(
	context.Context, string, string, models.Layout,
) error {
	r.ensured = true
	return nil
}

func (r *recordingOpenWorkspaceRunner) EnsureAndAttach(
	context.Context, string, string, models.Layout, bool,
) error {
	r.attached = true
	return nil
}

func TestFindEntryByPath(t *testing.T) {
	a := &discovery.GlobalWorktreeEntry{
		Path:   "/wt/a",
		Branch: "a",
		RepositoryInfo: &url.RepositoryInfo{
			Host: "github.com", Owner: "o", Repository: "r", FullPath: "github.com/o/r",
		},
	}
	b := &discovery.GlobalWorktreeEntry{Path: "/wt/b", Branch: "b"}
	entries := []*discovery.GlobalWorktreeEntry{a, b}

	assert.Same(t, a, findEntryByPath(entries, "/wt/a"))
	assert.Nil(t, findEntryByPath(entries, "/wt/missing"))
}

func TestShouldLoadTargetDefault(t *testing.T) {
	cases := []struct {
		name         string
		layoutFlag   string
		selectLayout bool
		want         bool
	}{
		{"neither flag set", "", false, true},
		{"explicit layout flag", "focus", false, false},
		{"select-layout flag", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldLoadTargetDefault(tc.layoutFlag, tc.selectLayout))
		})
	}
}

func TestOpenStartSessionFlagGroups(t *testing.T) {
	startSession := openCmd.Flags().Lookup("start-session")
	selectLayout := openCmd.Flags().Lookup("select-layout")
	layout := openCmd.Flags().Lookup("layout")
	require.NotNil(t, startSession)
	require.NotNil(t, selectLayout)
	require.NotNil(t, layout)

	oldStartSessionChanged := startSession.Changed
	oldSelectLayoutChanged := selectLayout.Changed
	oldLayoutChanged := layout.Changed
	t.Cleanup(func() {
		startSession.Changed = oldStartSessionChanged
		selectLayout.Changed = oldSelectLayoutChanged
		layout.Changed = oldLayoutChanged
	})

	startSession.Changed = true
	selectLayout.Changed = true
	layout.Changed = false
	require.Error(t, openCmd.ValidateFlagGroups())

	selectLayout.Changed = false
	layout.Changed = true
	require.NoError(t, openCmd.ValidateFlagGroups())
}

func TestResolveOpenWorktreeAcceptsExactPrimaryPathOutsideGlobalBase(
	t *testing.T,
) {
	t.Setenv("GIT_AUTHOR_NAME", "Test User")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test User")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	repo := filepath.Join(t.TempDir(), "registered", "widget")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, exec.Command("git", "init", "-b", "main", repo).Run())
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "README.md"),
		[]byte("# widget\n"),
		0o644,
	))
	gitCommand := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		require.NoError(t, cmd.Run())
	}
	gitCommand("add", "README.md")
	gitCommand("commit", "-m", "initial")

	entry, requestedPath, err := resolveOpenWorktree(
		&CommandContext{Config: &models.Config{
			Worktree: models.WorktreeConfig{
				BaseDir: filepath.Join(t.TempDir(), "global-worktrees"),
			},
			Projects: []models.Project{{
				Repository: "github.com/acme/widget",
				Path:       repo,
			}},
		}},
		[]string{repo},
		false,
	)

	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, repo, requestedPath)
	assert.Equal(t, utils.CanonicalPath(repo), utils.CanonicalPath(entry.Path))
	assert.True(t, entry.IsMain)
	require.NotNil(t, entry.RepositoryInfo)
	assert.Equal(t, "github.com/acme/widget", entry.RepositoryInfo.FullPath)
}

func TestOpenSelectedWorktreeRefusesProtectedPullRequestWorkspace(
	t *testing.T,
) {
	t.Setenv("KWT_HOME", t.TempDir())
	workspacePath := t.TempDir()
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-32"] = pullrequest.Provenance{
				Workspace: pullrequest.Workspace{Path: workspacePath},
			}
			return nil
		},
	))

	err := openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		&discovery.GlobalWorktreeEntry{
			Path: workspacePath,
			RepositoryInfo: &url.RepositoryInfo{
				FullPath: "github.com/acme/widget",
			},
		},
		nil,
		false,
		false,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kwt pr attach")
}

func TestOpenSelectedWorktreeStartsSessionWithoutAttaching(t *testing.T) {
	runner := &recordingOpenWorkspaceRunner{}
	var protectedNames []string
	var acknowledgedPath string
	oldNewRunner := newOpenWorkspaceRunner
	oldAcknowledge := acknowledgeRemoteSourcePath
	oldLayout := openLayout
	oldSelectLayout := openSelectLayout
	t.Cleanup(func() {
		newOpenWorkspaceRunner = oldNewRunner
		acknowledgeRemoteSourcePath = oldAcknowledge
		openLayout = oldLayout
		openSelectLayout = oldSelectLayout
	})
	newOpenWorkspaceRunner = func(names []string) openWorkspaceRunner {
		protectedNames = append([]string(nil), names...)
		return runner
	}
	acknowledgeRemoteSourcePath = func(path string) error {
		acknowledgedPath = path
		return nil
	}
	openLayout = tmux.BlankLayoutName
	openSelectLayout = false
	worktreePath := t.TempDir()

	err := openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{
			Fleet: models.FleetConfig{TokenEnv: "CUSTOM_FLEET_TOKEN"},
		}},
		&discovery.GlobalWorktreeEntry{
			Path:   worktreePath,
			Branch: "feature",
			RepositoryInfo: &url.RepositoryInfo{
				FullPath: "github.com/acme/widget",
			},
		},
		nil,
		true,
		false,
	)

	require.NoError(t, err)
	assert.Equal(t, worktreePath, acknowledgedPath)
	assert.True(t, runner.ensured)
	assert.False(t, runner.attached)
	assert.ElementsMatch(
		t,
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN", "CUSTOM_FLEET_TOKEN"},
		protectedNames,
	)
}

func TestOpenSelectedWorktreeAcknowledgesPersistedRemoteSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	worktreePath := t.TempDir()
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:                   worktreePath,
		Branch:                 "feature/remote",
		UnreviewedRemoteSource: true,
	}))

	runner := &recordingOpenWorkspaceRunner{}
	oldNewRunner := newOpenWorkspaceRunner
	oldLayout := openLayout
	oldSelectLayout := openSelectLayout
	t.Cleanup(func() {
		newOpenWorkspaceRunner = oldNewRunner
		openLayout = oldLayout
		openSelectLayout = oldSelectLayout
	})
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	openLayout = tmux.BlankLayoutName
	openSelectLayout = false

	err = openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		&discovery.GlobalWorktreeEntry{
			Path:   worktreePath,
			Branch: "feature/remote",
			RepositoryInfo: &url.RepositoryInfo{
				FullPath: "github.com/acme/widget",
			},
		},
		nil,
		true,
		false,
	)

	require.NoError(t, err)
	reloaded, err := registry.New()
	require.NoError(t, err)
	assert.False(t, reloaded.IsUnreviewedRemoteSource(worktreePath))
	assert.True(t, runner.ensured)
}

func TestOpenSelectedWorktreeStartSessionDoesNotPromptForTargetTrust(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	repo := t.TempDir()
	gitInit := exec.Command("git", "init", "-b", "main", repo)
	require.NoError(t, gitInit.Run())
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".kwt.toml"),
		[]byte("[layouts]\ndefault = \"focus\"\n"),
		0o644,
	))

	runner := &recordingOpenWorkspaceRunner{}
	oldNewRunner := newOpenWorkspaceRunner
	oldLayout := openLayout
	oldSelectLayout := openSelectLayout
	t.Cleanup(func() {
		newOpenWorkspaceRunner = oldNewRunner
		openLayout = oldLayout
		openSelectLayout = oldSelectLayout
	})
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	openLayout = ""
	openSelectLayout = false

	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	oldStderr := os.Stderr
	os.Stderr = stderr
	err = openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		&discovery.GlobalWorktreeEntry{
			Path:   repo,
			Branch: "feature",
			RepositoryInfo: &url.RepositoryInfo{
				FullPath: "github.com/acme/widget",
			},
		},
		nil,
		true,
		true,
	)
	os.Stderr = oldStderr
	require.NoError(t, stderr.Close())
	output, readErr := os.ReadFile(stderr.Name())
	require.NoError(t, readErr)

	require.NoError(t, err)
	assert.NotContains(t, string(output), "Trust this file and load it?")
	assert.True(t, runner.ensured)
	assert.False(t, runner.attached)
}

// TestOpenCmdIsolatesFromCwdConfig guards the config-isolation invariant:
// openCmd overrides the root PersistentPreRunE (which merges the caller's cwd
// .kwt.toml) with a no-op, so opening another repo's worktree never inherits
// the current directory's config. If the override is removed, openCmd falls
// back to root's cwd merge -- this test fails because the field goes nil.
func TestOpenCmdIsolatesFromCwdConfig(t *testing.T) {
	require.NotNil(t, openCmd.PersistentPreRunE,
		"open must define its own PersistentPreRunE to bypass root's cwd merge")
	require.NoError(t, openCmd.PersistentPreRunE(openCmd, nil),
		"open's PersistentPreRunE must be a no-op that never errors")
}
