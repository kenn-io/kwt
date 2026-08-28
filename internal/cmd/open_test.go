package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type blockingOpenRemovalGuard struct {
	entered chan struct{}
	release chan struct{}
}

func (g *blockingOpenRemovalGuard) Quiesce(
	_ context.Context,
	_ tmux.RemovalSessionCondition,
) (tmux.RemovalSessionLease, error) {
	close(g.entered)
	<-g.release
	return openTestRemovalLease{}, nil
}

type openTestRemovalLease struct{}

func (openTestRemovalLease) Terminate(context.Context) error { return nil }
func (openTestRemovalLease) Resume() error                   { return nil }

type signalingOpenWorkspaceRunner struct {
	ensure chan struct{}
}

func (r *signalingOpenWorkspaceRunner) Establish(
	context.Context, string, string, models.Layout,
) (tmux.SessionEndpoint, error) {
	r.ensure <- struct{}{}
	return tmux.SessionEndpoint{}, nil
}

func (r *signalingOpenWorkspaceRunner) EstablishWithGeneration(
	ctx context.Context,
	sessionName, workingDirectory, _ string,
	layout models.Layout,
) (tmux.SessionEndpoint, error) {
	return r.Establish(ctx, sessionName, workingDirectory, layout)
}

func (r *signalingOpenWorkspaceRunner) Attach(context.Context, tmux.SessionEndpoint) error {
	return nil
}

type recordingOpenWorkspaceRunner struct {
	ensured          bool
	attached         bool
	sessionName      string
	workingDirectory string
	generation       string
	layout           models.Layout
	endpoint         tmux.SessionEndpoint
	attachedEndpoint tmux.SessionEndpoint
}

func TestOpenSessionResultUsesEstablishedEndpoint(t *testing.T) {
	for _, endpoint := range []tmux.SessionEndpoint{
		testCanonicalSessionEndpoint("canonical"),
		{SessionName: "default-server"},
	} {
		got := openSessionResultFromEndpoint(endpoint)

		assert.Equal(t, endpoint.SessionName, got.SessionName)
		assert.Equal(t, endpoint.SocketName, got.TmuxSocketName)
		assert.Equal(t, models.TmuxAttachDirect, got.TmuxAttachMode)
	}
}

func TestOpenJSONRequiresStartSessionAndExactPath(t *testing.T) {
	require.Error(t, validateOpenOutputMode(nil, false, true))
	require.Error(t, validateOpenOutputMode([]string{"workspace"}, false, true))
	require.Error(t, validateOpenOutputMode(nil, true, true))
	require.NoError(t, validateOpenOutputMode([]string{"/work/widget"}, true, true))
}

func markCommandFlagsChanged(t *testing.T, cmd *cobra.Command, names ...string) {
	t.Helper()
	for _, name := range names {
		if cmd.Flags().Lookup(name) == nil {
			cmd.Flags().String(name, "", "")
		}
		cmd.Flags().Lookup(name).Changed = true
	}
}

func (r *recordingOpenWorkspaceRunner) Establish(
	_ context.Context, sessionName, workingDirectory string, layout models.Layout,
) (tmux.SessionEndpoint, error) {
	r.ensured = true
	r.sessionName = sessionName
	r.workingDirectory = workingDirectory
	r.layout = layout
	endpoint := r.endpoint
	if endpoint.SessionName == "" {
		endpoint = testCanonicalSessionEndpoint(sessionName)
	}
	r.endpoint = endpoint
	return endpoint, nil
}

func (r *recordingOpenWorkspaceRunner) EstablishWithGeneration(
	ctx context.Context,
	sessionName, workingDirectory, generation string,
	layout models.Layout,
) (tmux.SessionEndpoint, error) {
	endpoint, err := r.Establish(ctx, sessionName, workingDirectory, layout)
	r.generation = generation
	return endpoint, err
}

func (r *recordingOpenWorkspaceRunner) Attach(
	_ context.Context,
	endpoint tmux.SessionEndpoint,
) error {
	r.attached = true
	r.attachedEndpoint = endpoint
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
	isolateCommandTestHome(t)
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
			resolveDirectorySessions = func(workspaces []models.Workspace) ([]tmux.WorkspaceSession, error) {
				return testDirectorySessions(workspaces, ""), nil
			}
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
			assert.True(t, runner.ensured)
			assert.Equal(t, !tt.startSession, runner.attached)
		})
	}
}

func TestOpenSelectedDirectoryWorkspaceRequestsCanonicalSession(t *testing.T) {
	isolateCommandTestHome(t)
	resetWorkspaceCommandDeps(t)
	workspace := models.Workspace{Name: "renamed", Path: t.TempDir()}
	liveName := tmux.DirWorkspaceSessionName("old-name", workspace.Path)
	resolveDirectorySessions = func(workspaces []models.Workspace) ([]tmux.WorkspaceSession, error) {
		return testDirectorySessions(workspaces, liveName), nil
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
	assert.Equal(
		t,
		tmux.DirWorkspaceSessionName(workspace.Name, workspace.Path),
		runner.sessionName,
	)
}

func TestOpenAttachesToEndpointReturnedByEstablishment(t *testing.T) {
	isolateCommandTestHome(t)
	resetWorkspaceCommandDeps(t)
	workspace := models.Workspace{Name: "notes", Path: t.TempDir()}
	resolveDirectorySessions = func(workspaces []models.Workspace) ([]tmux.WorkspaceSession, error) {
		return testDirectorySessions(workspaces, ""), nil
	}
	want := tmux.SessionEndpoint{
		SessionName: tmux.DirWorkspaceSessionName(workspace.Name, workspace.Path),
		SocketName:  tmux.KWTServerSocketName,
	}
	runner := &recordingOpenWorkspaceRunner{endpoint: want}
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
		false,
		false,
	)

	require.NoError(t, err)
	assert.Equal(t, want, runner.attachedEndpoint)
}

func TestOpenStartSessionJSONWritesEstablishedEndpointOnly(t *testing.T) {
	isolateCommandTestHome(t)
	resetWorkspaceCommandDeps(t)
	workspace := models.Workspace{Name: "notes", Path: t.TempDir()}
	resolveDirectorySessions = func(workspaces []models.Workspace) ([]tmux.WorkspaceSession, error) {
		return testDirectorySessions(workspaces, ""), nil
	}
	want := tmux.SessionEndpoint{
		SessionName: tmux.DirWorkspaceSessionName(workspace.Name, workspace.Path),
		SocketName:  tmux.KWTServerSocketName,
	}
	runner := &recordingOpenWorkspaceRunner{endpoint: want}
	originalRunner := newOpenWorkspaceRunner
	originalLayout, originalJSON := openLayout, openJSON
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	openLayout, openJSON = tmux.BlankLayoutName, true
	t.Cleanup(func() {
		newOpenWorkspaceRunner = originalRunner
		openLayout, openJSON = originalLayout, originalJSON
	})
	command := &cobra.Command{}
	var stdout bytes.Buffer
	command.SetOut(&stdout)

	err := openSelectedDirectoryWorkspaceWithResult(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		workspace,
		nil,
		true,
		false,
		openResultCallback(command),
	)

	require.NoError(t, err)
	assert.False(t, runner.attached)
	assert.True(t, json.Valid(stdout.Bytes()))
	var got openSessionResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, openSessionResultFromEndpoint(want), got)
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
	resolveDirectorySessions = func(workspaces []models.Workspace) ([]tmux.WorkspaceSession, error) {
		return testDirectorySessions(workspaces, ""), nil
	}
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
	isolateCommandTestHome(t)
	for _, startSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "attach", true: "start only"}[startSession], func(t *testing.T) {
			resetWorkspaceCommandDeps(t)
			workspace := models.Workspace{Name: "notes", Path: t.TempDir()}
			resolveDirectorySessions = func(workspaces []models.Workspace) ([]tmux.WorkspaceSession, error) {
				return testDirectorySessions(workspaces, ""), nil
			}
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
			assert.True(t, runner.ensured)
			assert.Equal(t, !startSession, runner.attached)
		})
	}
}

func TestRunOpenWithContextRejectsGuardedDirectoryWorkspace(t *testing.T) {
	workspace := models.Workspace{Name: "notes", Path: t.TempDir()}
	originalRepository := openExpectedRepository
	originalRegistration := openExpectedRegistration
	originalGeneration := openExpectedGeneration
	originalSession := openExpectedSession
	openExpectedRepository = "github.com/acme/widget"
	openExpectedRegistration = "v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	openExpectedGeneration = "0123456789abcdef0123456789abcdef"
	openExpectedSession = "widget-topic"
	t.Cleanup(func() {
		openExpectedRepository = originalRepository
		openExpectedRegistration = originalRegistration
		openExpectedGeneration = originalGeneration
		openExpectedSession = originalSession
	})
	cmd, _, _ := fleetTestCommand()
	markCommandFlagsChanged(
		t,
		cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
	)
	cmd.SetContext(context.Background())

	err := runOpenWithContext(
		cmd,
		[]string{workspace.Path},
		&CommandContext{Config: &models.Config{
			Workspaces: []models.Workspace{workspace},
		}},
	)

	assert.True(t, service.IsCode(err, service.InvalidRequest))
	assert.Contains(t, err.Error(), "registered Git worktrees")
}

func TestRunOpenWithContextRejectsExplicitEmptyExpectedFlags(t *testing.T) {
	workspace := models.Workspace{Name: "notes", Path: t.TempDir()}
	cmd, _, _ := fleetTestCommand()
	markCommandFlagsChanged(
		t,
		cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
	)
	cmd.SetContext(context.Background())

	err := runOpenWithContext(
		cmd,
		[]string{workspace.Path},
		&CommandContext{Config: &models.Config{Workspaces: []models.Workspace{workspace}}},
	)

	assert.True(t, service.IsCode(err, service.InvalidRequest))
}

func TestRunOpenWithContextWithoutArgumentsUsesPickerFlow(t *testing.T) {
	originalStartSession := openStartSession
	originalRepository := openExpectedRepository
	originalRegistration := openExpectedRegistration
	originalGeneration := openExpectedGeneration
	originalSession := openExpectedSession
	t.Cleanup(func() {
		openStartSession = originalStartSession
		openExpectedRepository = originalRepository
		openExpectedRegistration = originalRegistration
		openExpectedGeneration = originalGeneration
		openExpectedSession = originalSession
	})
	openStartSession = false
	openExpectedRepository = ""
	openExpectedRegistration = ""
	openExpectedGeneration = ""
	openExpectedSession = ""
	cmd, _, _ := fleetTestCommand()
	cmd.SetContext(context.Background())

	err := runOpenWithContext(
		cmd,
		nil,
		&CommandContext{Config: &models.Config{
			Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
		}},
	)

	require.NoError(t, err)
}

func TestRunOpenWithContextReportsGuardedDisappearanceAsRegistrationChanged(
	t *testing.T,
) {
	for _, startSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "attach", true: "start only"}[startSession], func(t *testing.T) {
			worktreePath := filepath.Join(t.TempDir(), "disappeared-worktree")
			originalDiscover := discoverOpenWorktree
			originalStartSession := openStartSession
			originalRepository := openExpectedRepository
			originalRegistration := openExpectedRegistration
			originalGeneration := openExpectedGeneration
			originalSession := openExpectedSession
			t.Cleanup(func() {
				discoverOpenWorktree = originalDiscover
				openStartSession = originalStartSession
				openExpectedRepository = originalRepository
				openExpectedRegistration = originalRegistration
				openExpectedGeneration = originalGeneration
				openExpectedSession = originalSession
			})
			discoverOpenWorktree = func(path string, _ []models.Project) (*discovery.GlobalWorktreeEntry, error) {
				assert.Equal(t, worktreePath, path)
				return nil, errors.New("worktree disappeared")
			}
			openStartSession = startSession
			openExpectedRepository = "github.com/acme/widget"
			openExpectedRegistration = "v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			openExpectedGeneration = "0123456789abcdef0123456789abcdef"
			openExpectedSession = "widget-topic"
			cmd, _, _ := fleetTestCommand()
			markCommandFlagsChanged(
				t,
				cmd,
				"expected-repository",
				"expected-registration",
				"expected-generation",
				"expected-session",
			)
			cmd.SetContext(context.Background())

			err := runOpenWithContext(
				cmd,
				[]string{worktreePath},
				&CommandContext{Config: &models.Config{}},
			)

			assert.True(t, service.IsCode(err, service.RegistrationChanged))
			assert.ErrorContains(t, err, "worktree changed before it was opened")
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
	initCommandTestConfig(t, t.TempDir())
	worktreePath := newTUITestRepo(t)
	generation, err := git.New(worktreePath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)

	err = openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{
			Fleet: models.FleetConfig{TokenEnv: "CUSTOM_FLEET_TOKEN"},
		}},
		&discovery.GlobalWorktreeEntry{
			Path:       worktreePath,
			Branch:     "feature",
			Generation: generation,
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

func TestOpenSelectedWorktreeUsesBranchObservedInsideLifecycleGuard(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "branch-switch-open")
	runTUITestGit(t, repoPath, "branch", "feature/original")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/original")
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	repositoryInfo, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)
	initCommandTestConfig(t, t.TempDir())

	runner := &recordingOpenWorkspaceRunner{}
	originalRunner := newOpenWorkspaceRunner
	originalBeforeAcquire := beforeProjectGuardAcquire
	originalLayout := openLayout
	originalExpectedRepository := openExpectedRepository
	t.Cleanup(func() {
		newOpenWorkspaceRunner = originalRunner
		beforeProjectGuardAcquire = originalBeforeAcquire
		openLayout = originalLayout
		openExpectedRepository = originalExpectedRepository
	})
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	openLayout = tmux.BlankLayoutName
	openExpectedRepository = ""
	switched := false
	beforeProjectGuardAcquire = func() {
		if switched {
			return
		}
		switched = true
		runTUITestGit(t, worktreePath, "switch", "-c", "feature/current")
	}

	err = openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		&discovery.GlobalWorktreeEntry{
			Path: worktreePath, Branch: "feature/original", Generation: generation,
			RepositoryInfo: repositoryInfo,
		},
		nil,
		true,
		false,
	)

	require.NoError(t, err)
	assert.True(t, runner.ensured)
	assert.Equal(t, tmux.WorkspaceSessionName(
		repositoryInfo, "feature/current", worktreePath,
	), runner.sessionName)
}

func TestDirectOpenCannotRaceGuardedRemoval(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "open-race")
	runTUITestGit(t, repoPath, "branch", "open-race")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "open-race")
	generation, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	initCommandTestConfig(t, t.TempDir())
	home := os.Getenv("KWT_HOME")
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)
	removalSessionName, _, err := lifecycle.ResolveCurrentWorktreeSessionIdentity(
		context.Background(), worktreePath, nil, nil,
	)
	require.NoError(t, err)
	removalGuard := &blockingOpenRemovalGuard{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	removalDone := make(chan error, 1)
	go func() {
		_, removeErr := lifecycle.NewRemovalService(lifecycle.RemovalServiceOptions{
			Home: home, SessionGuard: removalGuard,
		}).Remove(context.Background(), lifecycle.RemovalRequest{
			RepositoryPath: repoPath,
			Path:           worktreePath, ExpectedGeneration: generation,
			Expansion: expansion,
			Session: &tmux.RemovalSessionCondition{
				SessionName: removalSessionName, Absent: true,
			},
		})
		removalDone <- removeErr
	}()
	<-removalGuard.entered

	runner := &signalingOpenWorkspaceRunner{ensure: make(chan struct{}, 1)}
	originalRunner := newOpenWorkspaceRunner
	originalLayout := openLayout
	originalExpectedRepository := openExpectedRepository
	t.Cleanup(func() {
		newOpenWorkspaceRunner = originalRunner
		openLayout = originalLayout
		openExpectedRepository = originalExpectedRepository
	})
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	openLayout = tmux.BlankLayoutName
	openExpectedRepository = ""
	openDone := make(chan error, 1)
	go func() {
		openDone <- openSelectedWorktree(
			context.Background(),
			&CommandContext{Config: &models.Config{}},
			&discovery.GlobalWorktreeEntry{
				Path: worktreePath, Branch: "open-race", Generation: generation,
				RepositoryInfo: &url.RepositoryInfo{FullPath: repoPath},
			},
			nil,
			true,
			false,
		)
	}()

	select {
	case <-runner.ensure:
		t.Fatal("direct open established a session during guarded removal")
	case <-time.After(100 * time.Millisecond):
	}
	close(removalGuard.release)
	require.NoError(t, <-removalDone)
	require.Error(t, <-openDone)
	select {
	case <-runner.ensure:
		t.Fatal("direct open established a session after its worktree was removed")
	default:
	}
}

func TestExpectedOpenRejectsReplacementBeforeSessionEnsure(t *testing.T) {
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	configPath := filepath.Join(os.Getenv("KWT_HOME"), "config.toml")
	file, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = fmt.Fprintf(
		file,
		"\n[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = %q\n",
		repoPath,
	)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	snapshot, err := config.LoadGlobalSnapshotAt(os.Getenv("KWT_HOME"))
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	fingerprint, err := snapshot.Projects[0].Fingerprint()
	require.NoError(t, err)

	const originalGeneration = "0123456789abcdef0123456789abcdef"
	const replacementGeneration = "fedcba9876543210fedcba9876543210"
	entry := &discovery.GlobalWorktreeEntry{
		Path:       repoPath,
		Branch:     "main",
		Generation: originalGeneration,
		RepositoryInfo: &url.RepositoryInfo{
			FullPath: "github.com/acme/widget",
		},
	}
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:                   repoPath,
		Branch:                 "main",
		UnreviewedRemoteSource: true,
	}))
	expectedSession := tmux.WorkspaceSessionName(
		entry.RepositoryInfo,
		entry.Branch,
		entry.Path,
	)
	runner := &recordingOpenWorkspaceRunner{}
	originalRunner := newOpenWorkspaceRunner
	originalDiscover := discoverOpenWorktree
	originalBeforeAcquire := beforeProjectGuardAcquire
	originalLayout := openLayout
	originalExpectedRepository := openExpectedRepository
	originalExpectedRegistration := openExpectedRegistration
	originalExpectedGeneration := openExpectedGeneration
	originalExpectedSession := openExpectedSession
	t.Cleanup(func() {
		newOpenWorkspaceRunner = originalRunner
		discoverOpenWorktree = originalDiscover
		beforeProjectGuardAcquire = originalBeforeAcquire
		openLayout = originalLayout
		openExpectedRepository = originalExpectedRepository
		openExpectedRegistration = originalExpectedRegistration
		openExpectedGeneration = originalExpectedGeneration
		openExpectedSession = originalExpectedSession
	})
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	openLayout = tmux.BlankLayoutName
	openExpectedRepository = "github.com/acme/widget"
	openExpectedRegistration = fingerprint
	openExpectedGeneration = originalGeneration
	openExpectedSession = expectedSession
	replaced := false
	beforeProjectGuardAcquire = func() { replaced = true }
	discoverOpenWorktree = func(path string, projects []models.Project) (*discovery.GlobalWorktreeEntry, error) {
		assert.True(t, replaced)
		assert.Equal(t, repoPath, path)
		require.Len(t, projects, 1)
		return &discovery.GlobalWorktreeEntry{
			Path:       repoPath,
			Branch:     "main",
			Generation: replacementGeneration,
			RepositoryInfo: &url.RepositoryInfo{
				FullPath: "github.com/acme/widget",
			},
		}, nil
	}

	err = openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		entry,
		nil,
		true,
		false,
	)

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, runner.ensured)
	assert.False(t, runner.attached)
	reloaded, err := registry.New()
	require.NoError(t, err)
	assert.True(t, reloaded.IsUnreviewedRemoteSource(repoPath))
}

func TestExpectedOpenRejectsStaleDiscoveryAfterWorktreeReplacement(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "guarded-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "feature/original", worktreePath)
	initCommandTestConfig(t, t.TempDir())
	configPath := filepath.Join(os.Getenv("KWT_HOME"), "config.toml")
	file, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = fmt.Fprintf(
		file,
		"\n[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = %q\n",
		repoPath,
	)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	snapshot, err := config.LoadGlobalSnapshotAt(os.Getenv("KWT_HOME"))
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	fingerprint, err := snapshot.Projects[0].Fingerprint()
	require.NoError(t, err)

	entry := &discovery.GlobalWorktreeEntry{
		Path:       worktreePath,
		Branch:     "feature/original",
		Generation: tuiTestWorktreeGeneration(t, repoPath, worktreePath),
		RepositoryInfo: &url.RepositoryInfo{
			FullPath: "github.com/acme/widget",
		},
	}
	expectedSession := tmux.WorkspaceSessionName(
		entry.RepositoryInfo,
		entry.Branch,
		entry.Path,
	)
	runner := &recordingOpenWorkspaceRunner{}
	originalRunner := newOpenWorkspaceRunner
	originalDiscover := discoverOpenWorktree
	originalLayout := openLayout
	originalExpectedRepository := openExpectedRepository
	originalExpectedRegistration := openExpectedRegistration
	originalExpectedGeneration := openExpectedGeneration
	originalExpectedSession := openExpectedSession
	t.Cleanup(func() {
		newOpenWorkspaceRunner = originalRunner
		discoverOpenWorktree = originalDiscover
		openLayout = originalLayout
		openExpectedRepository = originalExpectedRepository
		openExpectedRegistration = originalExpectedRegistration
		openExpectedGeneration = originalExpectedGeneration
		openExpectedSession = originalExpectedSession
	})
	newOpenWorkspaceRunner = func([]string) openWorkspaceRunner { return runner }
	discoverOpenWorktree = func(string, []models.Project) (*discovery.GlobalWorktreeEntry, error) {
		return entry, nil
	}
	openLayout = tmux.BlankLayoutName
	openExpectedRepository = "github.com/acme/widget"
	openExpectedRegistration = fingerprint
	openExpectedGeneration = entry.Generation
	openExpectedSession = expectedSession

	runTUITestGit(t, repoPath, "worktree", "remove", "--force", worktreePath)
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "feature/replacement", worktreePath)
	replacementGeneration := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	require.NotEqual(t, entry.Generation, replacementGeneration)

	err = openSelectedWorktree(
		context.Background(),
		&CommandContext{Config: &models.Config{}},
		entry,
		nil,
		true,
		false,
	)

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, runner.ensured)
}

func TestOpenSelectedWorktreeAcknowledgesPersistedRemoteSource(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "feature-remote")
	runTUITestGit(t, repoPath, "branch", "feature/remote")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/remote")
	generation, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:                   worktreePath,
		Branch:                 "feature/remote",
		Generation:             generation,
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
			Path:       worktreePath,
			Branch:     "feature/remote",
			Generation: generation,
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
	generation, err := git.New(repo).WorktreeGeneration(repo)
	require.NoError(t, err)

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
			Path:       repo,
			Branch:     "feature",
			Generation: generation,
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
