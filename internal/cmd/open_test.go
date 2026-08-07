package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

type recordingOpenWorkspaceRunner struct {
	ensured          bool
	attached         bool
	sessionName      string
	workingDirectory string
	layout           models.Layout
	insideTmux       bool
}

func (r *recordingOpenWorkspaceRunner) Ensure(
	_ context.Context, sessionName, workingDirectory string, layout models.Layout,
) error {
	r.ensured = true
	r.sessionName = sessionName
	r.workingDirectory = workingDirectory
	r.layout = layout
	return nil
}

func (r *recordingOpenWorkspaceRunner) EnsureAndAttach(
	_ context.Context,
	sessionName string,
	workingDirectory string,
	layout models.Layout,
	insideTmux bool,
) error {
	r.attached = true
	r.sessionName = sessionName
	r.workingDirectory = workingDirectory
	r.layout = layout
	r.insideTmux = insideTmux
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

func TestOpenSelectedDirectoryWorkspaceEnsuresOrAttaches(t *testing.T) {
	tests := []struct {
		name         string
		startSession bool
	}{
		{name: "attach", startSession: false},
		{name: "start only", startSession: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetWorkspaceCommandDeps(t)
			workspace := models.Workspace{Name: "notes", Path: t.TempDir()}
			listWorkspaceSessions = func() ([]string, error) { return nil, nil }
			runner := &recordingOpenWorkspaceRunner{}
			originalRunner := newOpenWorkspaceRunner
			originalLayout := openLayout
			originalSelectLayout := openSelectLayout
			newOpenWorkspaceRunner = func([]string) openWorkspaceRunner {
				return runner
			}
			openLayout = tmux.BlankLayoutName
			openSelectLayout = false
			t.Cleanup(func() {
				newOpenWorkspaceRunner = originalRunner
				openLayout = originalLayout
				openSelectLayout = originalSelectLayout
			})

			err := openSelectedDirectoryWorkspace(
				context.Background(),
				&CommandContext{Config: &models.Config{}},
				workspace,
				func([]models.Layout) (models.Layout, error) {
					return models.Layout{}, nil
				},
				tt.startSession,
				false,
			)

			require.NoError(t, err)
			assert.Equal(t, workspace.Path, runner.workingDirectory)
			assert.Equal(
				t,
				tmux.DirWorkspaceSessionName(workspace.Name, workspace.Path),
				runner.sessionName,
			)
			assert.Equal(t, tt.startSession, runner.ensured)
			assert.Equal(t, !tt.startSession, runner.attached)
		})
	}
}

func TestOpenSelectedDirectoryWorkspaceUsesRenamedLiveSession(t *testing.T) {
	resetWorkspaceCommandDeps(t)
	workspace := models.Workspace{Name: "renamed", Path: t.TempDir()}
	liveName := tmux.DirWorkspaceSessionName("old-name", workspace.Path)
	listWorkspaceSessions = func() ([]string, error) {
		return []string{liveName}, nil
	}
	runner := &recordingOpenWorkspaceRunner{}
	originalRunner := newOpenWorkspaceRunner
	originalLayout := openLayout
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	openLayout = tmux.BlankLayoutName
	t.Cleanup(func() {
		newOpenWorkspaceRunner = originalRunner
		openLayout = originalLayout
	})

	err := openSelectedDirectoryWorkspace(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		workspace,
		nil,
		true,
		false,
	)

	require.NoError(t, err)
	assert.Equal(t, liveName, runner.sessionName)
}

func TestOpenSelectedDirectoryWorkspaceUsesDirectoryLayoutDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KWT_HOME", filepath.Join(home, ".config", "kwt"))
	resetWorkspaceCommandDeps(t)
	workspace := models.Workspace{Name: "notes", Path: t.TempDir()}
	localTOML := []byte("[layouts]\ndefault = \"focus\"\n")
	localConfigPath := filepath.Join(workspace.Path, ".kwt.toml")
	require.NoError(t, os.WriteFile(localConfigPath, localTOML, 0o644))
	resolvedConfigPath, err := filepath.EvalSymlinks(localConfigPath)
	require.NoError(t, err)
	sum := sha256.Sum256(localTOML)
	trustStore, err := config.LoadTrustStore(
		filepath.Join(home, ".config", "kwt", "trusted_configs.json"),
	)
	require.NoError(t, err)
	require.NoError(t, trustStore.Add(resolvedConfigPath, hex.EncodeToString(sum[:])))
	listWorkspaceSessions = func() ([]string, error) { return nil, nil }
	runner := &recordingOpenWorkspaceRunner{}
	originalRunner := newOpenWorkspaceRunner
	originalLayout := openLayout
	originalSelectLayout := openSelectLayout
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	openLayout = ""
	openSelectLayout = false
	t.Cleanup(func() {
		newOpenWorkspaceRunner = originalRunner
		openLayout = originalLayout
		openSelectLayout = originalSelectLayout
	})

	err = openSelectedDirectoryWorkspace(
		context.Background(),
		&CommandContext{Config: &models.Config{
			Layouts: models.LayoutsConfig{Presets: []models.Layout{{
				Name: "focus", Panes: []string{""},
			}}},
		}},
		workspace,
		nil,
		true,
		false,
	)

	require.NoError(t, err)
	assert.Equal(t, "focus", runner.layout.Name)
}

func TestRunOpenWithContextChoosesRegisteredDirectory(t *testing.T) {
	for _, startSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "attach", true: "start only"}[startSession], func(t *testing.T) {
			resetWorkspaceCommandDeps(t)
			workspace := models.Workspace{Name: "notes", Path: t.TempDir()}
			listWorkspaceSessions = func() ([]string, error) { return nil, nil }
			runner := &recordingOpenWorkspaceRunner{}
			originalRunner := newOpenWorkspaceRunner
			originalLayout := openLayout
			originalSelectLayout := openSelectLayout
			originalStartSession := openStartSession
			newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
			openLayout = tmux.BlankLayoutName
			openSelectLayout = false
			openStartSession = startSession
			t.Cleanup(func() {
				newOpenWorkspaceRunner = originalRunner
				openLayout = originalLayout
				openSelectLayout = originalSelectLayout
				openStartSession = originalStartSession
			})

			cmd, _, _ := fleetTestCommand()
			cmd.SetContext(context.Background())
			err := runOpenWithContext(
				cmd,
				[]string{workspace.Path},
				&CommandContext{Config: &models.Config{
					Workspaces: []models.Workspace{workspace},
				}},
			)

			require.NoError(t, err)
			assert.Equal(t, workspace.Path, runner.workingDirectory)
			assert.Equal(t, startSession, runner.ensured)
			assert.Equal(t, !startSession, runner.attached)
		})
	}
}

func TestOpenStartSessionRequiresExactWorkspacePath(t *testing.T) {
	originalStartSession := openStartSession
	openStartSession = true
	t.Cleanup(func() { openStartSession = originalStartSession })
	cmd, _, _ := fleetTestCommand()
	cmd.SetContext(context.Background())

	err := runOpenWithContext(
		cmd,
		nil,
		&CommandContext{Config: &models.Config{}},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exact workspace path")
}

func TestRunOpenWithContextRejectsUnregisteredNonGitDirectory(t *testing.T) {
	originalLayout := openLayout
	originalStartSession := openStartSession
	openLayout = tmux.BlankLayoutName
	openStartSession = false
	t.Cleanup(func() {
		openLayout = originalLayout
		openStartSession = originalStartSession
	})
	cmd, _, _ := fleetTestCommand()
	cmd.SetContext(context.Background())
	path := t.TempDir()

	err := runOpenWithContext(
		cmd,
		[]string{path},
		&CommandContext{Config: &models.Config{
			Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
		}},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not resolve worktree")
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
	liveGeneration := "0123456789abcdef0123456789abcdef"
	stubPRWorkspaceGeneration(t, workspacePath, liveGeneration)
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-32"] = pullrequest.Provenance{Workspace: pullrequest.Workspace{
				Path: workspacePath, Generation: liveGeneration,
			}}
			return nil
		},
	))

	err := openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		&discovery.GlobalWorktreeEntry{
			Path:       workspacePath,
			Generation: "fedcba9876543210fedcba9876543210",
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
