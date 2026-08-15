package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/template"
	"go.kenn.io/kwt/internal/tmux"
	urlutil "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type fakePRService struct {
	prs         []pullrequest.PullRequest
	result      pullrequest.ImportResult
	listErr     error
	importErr   error
	gotState    string
	gotSelector string
	gotProject  pullrequest.Project
}

func (f *fakePRService) List(_ context.Context, project pullrequest.Project, state string) ([]pullrequest.PullRequest, error) {
	f.gotProject = project
	f.gotState = state
	return f.prs, f.listErr
}

func (f *fakePRService) Import(_ context.Context, project pullrequest.Project, selector string) (pullrequest.ImportResult, error) {
	f.gotProject = project
	f.gotSelector = selector
	return f.result, f.importErr
}

func withPRCommandDeps(t *testing.T, cfg *models.Config, service prService) {
	t.Helper()
	if os.Getenv("KWT_HOME") == "" {
		t.Setenv("KWT_HOME", t.TempDir())
	}
	home, err := config.CanonicalHome()
	require.NoError(t, err)
	if _, statErr := os.Stat(filepath.Join(home, "config.toml")); os.IsNotExist(statErr) {
		for _, project := range cfg.Projects {
			require.NoError(t, config.RegisterProject(project))
		}
	}
	oldLoad := loadPRConfig
	oldTargetLoad := loadPRTargetConfig
	oldNew := newPRService
	oldProvider := newPRGitHubProvider
	oldValidateRoot := validatePRProjectRoot
	oldProject := prProject
	oldState := prState
	oldStartSession := prStartSession
	oldAttachExpectedRepository := prAttachExpectedRepository
	oldAttachExpectedRegistration := prAttachExpectedRegistration
	oldAttachExpectedGeneration := prAttachExpectedGeneration
	oldAttachExpectedSession := prAttachExpectedSession
	oldAttachExpectedSocket := prAttachExpectedSocket
	oldValidateSessionConfig := validatePRWorkspaceSessionConfig
	oldStartWorkspaceSession := ensurePRWorkspaceSession
	oldAttachWorkspaceSession := attachExistingPRWorkspaceSession
	oldInspectProjectClone := inspectPRProjectClone
	oldReadWorkspaceGeneration := readPRWorkspaceGeneration
	oldWithWorkspaceGeneration := withPRWorkspaceGeneration
	t.Cleanup(func() {
		loadPRConfig = oldLoad
		loadPRTargetConfig = oldTargetLoad
		newPRService = oldNew
		newPRGitHubProvider = oldProvider
		validatePRProjectRoot = oldValidateRoot
		prProject = oldProject
		prState = oldState
		prStartSession = oldStartSession
		prAttachExpectedRepository = oldAttachExpectedRepository
		prAttachExpectedRegistration = oldAttachExpectedRegistration
		prAttachExpectedGeneration = oldAttachExpectedGeneration
		prAttachExpectedSession = oldAttachExpectedSession
		prAttachExpectedSocket = oldAttachExpectedSocket
		validatePRWorkspaceSessionConfig = oldValidateSessionConfig
		ensurePRWorkspaceSession = oldStartWorkspaceSession
		attachExistingPRWorkspaceSession = oldAttachWorkspaceSession
		inspectPRProjectClone = oldInspectProjectClone
		readPRWorkspaceGeneration = oldReadWorkspaceGeneration
		withPRWorkspaceGeneration = oldWithWorkspaceGeneration
	})
	loadPRConfig = func() (*models.Config, error) { return cfg, nil }
	loadPRTargetConfig = func(string, bool) (*models.Config, error) { return cfg, nil }
	newPRService = func(
		_ context.Context,
		_ *models.Config,
		project pullrequest.Project,
	) (prService, pullrequest.Project, error) {
		return service, project, nil
	}
	validatePRProjectRoot = func(project pullrequest.Project) (pullrequest.Project, error) { return project, nil }
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return pullrequest.Project{}, nil, nil
	}
	prProject = "widget"
	prState = "open"
	prStartSession = false
	prAttachExpectedRepository = ""
	prAttachExpectedRegistration = ""
	prAttachExpectedGeneration = ""
	prAttachExpectedSession = ""
	prAttachExpectedSocket = ""
	validatePRWorkspaceSessionConfig = func(*models.Config) error {
		return nil
	}
	ensurePRWorkspaceSession = func(
		_ context.Context,
		workspace pullrequest.Workspace,
		_ *models.Config,
	) (string, error) {
		return tmux.ProtectedWorkspaceSocketName(
			workspace.SessionName, workspace.Path,
		), nil
	}
	withPRWorkspaceGeneration = func(
		_ context.Context,
		_, _, _ string,
		establish func() error,
	) error {
		return establish()
	}
}

func stubPRWorkspaceGeneration(t *testing.T, path, generation string) {
	t.Helper()
	oldRead := readPRWorkspaceGeneration
	t.Cleanup(func() { readPRWorkspaceGeneration = oldRead })
	readPRWorkspaceGeneration = func(_, gotPath string) (string, error) {
		assert.Equal(t, path, gotPath)
		return generation, nil
	}
}

func TestRunPRImportValidatesSelectorBeforeAuthentication(t *testing.T) {
	withPRCommandDeps(t, testPRConfig(), &fakePRService{})
	called := false
	newPRService = func(
		_ context.Context,
		_ *models.Config,
		project pullrequest.Project,
	) (prService, pullrequest.Project, error) {
		called = true
		return nil, project, pullrequest.NewError(
			pullrequest.CodeAuthentication,
			"authentication required",
			false,
			nil,
		)
	}
	cmd, stdout, _ := prTestCommand()

	err := runPRImport(cmd, []string{"invalid"})

	assertPRCode(t, err, pullrequest.CodeInvalidSelector)
	assert.False(t, called)
	var envelope pullrequest.ErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, pullrequest.CodeInvalidSelector, envelope.Error.Code)
}

func TestPRArgumentValidationUsesStructuredErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   cobra.PositionalArgs
		args []string
	}{
		{name: "unexpected list argument", fn: prNoArgs, args: []string{"extra"}},
		{name: "missing import selector", fn: prExactArgs(1), args: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, stdout, stderr := prTestCommand()
			err := tc.fn(cmd, tc.args)
			var exitErr *prCommandError
			require.ErrorAs(t, err, &exitErr)
			assert.Equal(t, 2, exitErr.ExitCode())
			var envelope pullrequest.ErrorEnvelope
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
			assert.Equal(t, pullrequest.CodeInvalidSelector, envelope.Error.Code)
			assert.Contains(t, stderr.String(), "invalid_pull_request_selector")
		})
	}
}

func TestPRFlagValidationUsesStructuredErrors(t *testing.T) {
	cmd, stdout, stderr := prTestCommand()
	err := prCmd.FlagErrorFunc()(cmd, errors.New("unknown flag: --bogus"))

	var exitErr *prCommandError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 2, exitErr.ExitCode())
	var envelope pullrequest.ErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, pullrequest.CodeInvalidSelector, envelope.Error.Code)
	assert.Contains(t, stderr.String(), "invalid_pull_request_selector")
}

func TestPreparePRServiceLoadsSelectedTargetConfiguration(t *testing.T) {
	global := testPRConfig()
	target := testPRConfig()
	target.Worktree.BaseDir = "/target/worktrees"
	withPRCommandDeps(t, global, &fakePRService{})
	var loadedPath string
	loadPRTargetConfig = func(path string, interactive bool) (*models.Config, error) {
		loadedPath = path
		assert.False(t, interactive)
		return target, nil
	}
	var received *models.Config
	newPRService = func(
		_ context.Context,
		cfg *models.Config,
		project pullrequest.Project,
	) (prService, pullrequest.Project, error) {
		received = cfg
		return &fakePRService{}, project, nil
	}

	project, err := preparePRProject()
	require.NoError(t, err)
	_, _, _, err = preparePRService(context.Background(), project)

	require.NoError(t, err)
	assert.Equal(t, "/repos/widget", loadedPath)
	assert.Same(t, target, received)
}

func TestPreparePRServiceRejectsPathOutsideMainRepositoryRootBeforeLoadingConfig(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main", repo)
	require.NoError(t, cmd.Run())
	subdir := filepath.Join(repo, "nested")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	oldValidateRoot := validatePRProjectRoot
	oldTargetLoad := loadPRTargetConfig
	t.Cleanup(func() {
		validatePRProjectRoot = oldValidateRoot
		loadPRTargetConfig = oldTargetLoad
	})
	validatePRProjectRoot = defaultValidatePRProjectRoot
	loaded := false
	loadPRTargetConfig = func(string, bool) (*models.Config, error) {
		loaded = true
		return testPRConfig(), nil
	}

	_, _, _, err := preparePRService(context.Background(), pullrequest.Project{
		Identity: "github.com/acme/widget", Name: "widget", Path: subdir,
	})

	assertPRCode(t, err, pullrequest.CodeRepositoryMismatch)
	assert.False(t, loaded)
}

func prTestCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd, stdout, stderr
}

func testPRConfig() *models.Config {
	return &models.Config{Projects: []models.Project{{
		Repository: "github.com/acme/widget", Name: "widget", Path: "/repos/widget",
	}}}
}

func TestPRCommandsSkipCallerLocalConfig(t *testing.T) {
	require.NotNil(t, prCmd.PersistentPreRunE)
	require.NoError(t, prCmd.PersistentPreRunE(prCmd, nil))
}

func TestPRConfigInitializationFailureUsesJSONContract(t *testing.T) {
	if os.Getenv("KWT_TEST_PR_CONFIG_INIT_FAILURE") == "1" {
		rootCmd.SetArgs([]string{"pr", "list", "--project", "widget"})
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		Execute()
		return
	}

	kwtHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte("invalid = [\n"), 0o600))
	cmd := exec.Command(os.Args[0], "-test.run=^TestPRConfigInitializationFailureUsesJSONContract$")
	cmd.Env = append(os.Environ(),
		"KWT_TEST_PR_CONFIG_INIT_FAILURE=1",
		"KWT_HOME="+kwtHome,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 9, exitErr.ExitCode())
	var envelope pullrequest.ErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, pullrequest.CodeWorkspaceCreation, envelope.Error.Code)
	assert.Contains(t, stderr.String(), "workspace_creation_failed")
}

func TestRunPRListWritesStructuredOutput(t *testing.T) {
	service := &fakePRService{prs: []pullrequest.PullRequest{{
		ID: "github:github.com/acme/widget#17", Number: 17, Title: "Improve widgets",
	}}}
	withPRCommandDeps(t, testPRConfig(), service)
	prState = "all"
	cmd, stdout, stderr := prTestCommand()

	err := runPRList(cmd, nil)

	require.NoError(t, err)
	var envelope struct {
		PullRequests []pullrequest.PullRequest `json:"pull_requests"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	require.Len(t, envelope.PullRequests, 1)
	assert.Equal(t, 17, envelope.PullRequests[0].Number)
	assert.Equal(t, "all", service.gotState)
	assert.Equal(t, "github.com/acme/widget", service.gotProject.Identity)
	assert.Empty(t, stderr.String())
}

func TestRunPRListUsesResolvedTransferredRepository(t *testing.T) {
	service := &fakePRService{}
	cfg := testPRConfig()
	cfg.Projects[0].Repository = "github.com/legacy/widget"
	withPRCommandDeps(t, cfg, service)
	newPRService = func(
		_ context.Context,
		_ *models.Config,
		project pullrequest.Project,
	) (prService, pullrequest.Project, error) {
		project.Identity = "github.com/acme/widget"
		return service, project, nil
	}
	cmd, _, _ := prTestCommand()

	err := runPRList(cmd, nil)

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", service.gotProject.Identity)
}

func TestRunPRImportUsesResolvedTransferredRepository(t *testing.T) {
	for _, selector := range []string{
		"1166",
		"https://github.com/legacy/widget/pull/1166",
		"https://github.com/acme/widget/pull/1166",
	} {
		t.Run(selector, func(t *testing.T) {
			service := &fakePRService{result: pullrequest.ImportResult{
				Status: pullrequest.ImportCreated,
				Workspace: pullrequest.Workspace{
					ID: "ws", Path: "/worktrees/ws", SessionName: "kwt-workspace-ws",
				},
			}}
			cfg := testPRConfig()
			cfg.Projects[0].Repository = "github.com/legacy/widget"
			withPRCommandDeps(t, cfg, service)
			newPRService = func(
				_ context.Context,
				_ *models.Config,
				project pullrequest.Project,
			) (prService, pullrequest.Project, error) {
				project.Identity = "github.com/acme/widget"
				return service, project, nil
			}
			cmd, _, _ := prTestCommand()

			err := runPRImport(cmd, []string{selector})

			require.NoError(t, err)
			assert.Equal(t, "github.com/acme/widget", service.gotProject.Identity)
			assert.Equal(t, "1166", service.gotSelector)
		})
	}
}

func TestRunPRImportRejectsSelectorOutsideRegisteredAndResolvedRepositories(t *testing.T) {
	service := &fakePRService{}
	cfg := testPRConfig()
	cfg.Projects[0].Repository = "github.com/legacy/widget"
	withPRCommandDeps(t, cfg, service)
	newPRService = func(
		_ context.Context,
		_ *models.Config,
		project pullrequest.Project,
	) (prService, pullrequest.Project, error) {
		project.Identity = "github.com/acme/widget"
		return service, project, nil
	}
	cmd, _, _ := prTestCommand()

	err := runPRImport(cmd, []string{"https://github.com/other/widget/pull/1166"})

	assertPRCode(t, err, pullrequest.CodeInvalidSelector)
	assert.Empty(t, service.gotSelector)
}

func TestDefaultNewPRServiceResolvesTransferredRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/legacy/widget", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"name":"widget",
			"full_name":"acme/widget",
			"clone_url":"https://github.com/acme/widget.git"
		}`)
	}))
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithURLs(&baseURL, nil))
	require.NoError(t, err)
	oldProvider := newPRGitHubProvider
	t.Cleanup(func() { newPRGitHubProvider = oldProvider })
	newPRGitHubProvider = func(context.Context) (*pullrequest.GitHubProvider, error) {
		return pullrequest.NewGitHubProvider(client), nil
	}
	project := pullrequest.Project{
		Identity: "github.com/legacy/widget",
		Name:     "widget",
		Path:     "/repos/widget",
	}

	service, resolved, err := defaultNewPRService(
		context.Background(),
		&models.Config{},
		project,
	)

	require.NoError(t, err)
	assert.NotNil(t, service)
	assert.Equal(t, "github.com/acme/widget", resolved.Identity)
}

func TestDefaultNewPRServiceRejectsNestedRepositoryIdentity(t *testing.T) {
	oldProvider := newPRGitHubProvider
	t.Cleanup(func() { newPRGitHubProvider = oldProvider })
	called := false
	newPRGitHubProvider = func(context.Context) (*pullrequest.GitHubProvider, error) {
		called = true
		return nil, errors.New("must not authenticate")
	}

	_, _, err := defaultNewPRService(
		context.Background(),
		&models.Config{},
		pullrequest.Project{
			Identity: "github.com/acme/team/widget",
			Name:     "widget",
			Path:     "/repos/widget",
		},
	)

	assertPRCode(t, err, pullrequest.CodeUnsupportedProvider)
	assert.False(t, called)
}

func TestRunPRImportWritesCreatedAndAlreadyImportedResults(t *testing.T) {
	for _, status := range []pullrequest.ImportStatus{pullrequest.ImportCreated, pullrequest.ImportExisting} {
		t.Run(string(status), func(t *testing.T) {
			service := &fakePRService{result: pullrequest.ImportResult{
				Status: status, Project: pullrequest.Project{Identity: "github.com/acme/widget"},
				Workspace: pullrequest.Workspace{ID: "ws", Path: "/worktrees/ws", SessionName: "kwt-workspace-ws"},
			}}
			withPRCommandDeps(t, testPRConfig(), service)
			cmd, stdout, _ := prTestCommand()

			err := runPRImport(cmd, []string{"https://github.com/acme/widget/pull/17"})

			require.NoError(t, err)
			var got pullrequest.ImportResult
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
			assert.Equal(t, status, got.Status)
			assert.Equal(t, "17", service.gotSelector)
			socketName := tmux.ProtectedWorkspaceSocketName(
				got.Workspace.SessionName,
				got.Workspace.Path,
			)
			assert.Equal(t, socketName, got.Workspace.TmuxSocketName)
			assert.Equal(
				t,
				socketName,
				tryRequireWorkspace(t, got.PullRequest.Workspace).TmuxSocketName,
			)
		})
	}
}

func TestProtectedPRWorkspaceSessionIgnoresConfiguredLayout(t *testing.T) {
	cfg := &models.Config{
		Layouts: models.LayoutsConfig{
			Default: "project",
			Presets: []models.Layout{{
				Name:    "project",
				Arrange: "tiled",
				Panes:   []string{"make run-from-imported-checkout"},
			}},
		},
	}

	layout, err := preparePRWorkspaceSessionLayout(cfg)

	require.NoError(t, err)
	assert.Equal(t, tmux.BlankLayout(), layout)
}

func TestRunPRImportStartsCanonicalWorkspaceSessionOnRequest(t *testing.T) {
	workspace := pullrequest.Workspace{
		ID: "ws", Path: "/worktrees/ws",
		SessionName: "kwt-workspace-ws",
	}
	service := &fakePRService{result: pullrequest.ImportResult{
		Status:    pullrequest.ImportCreated,
		Project:   pullrequest.Project{Identity: "github.com/acme/widget"},
		Workspace: workspace,
	}}
	cfg := testPRConfig()
	withPRCommandDeps(t, cfg, service)
	prStartSession = true
	var started bool
	ensurePRWorkspaceSession = func(
		_ context.Context,
		got pullrequest.Workspace,
		gotConfig *models.Config,
	) (string, error) {
		started = true
		assert.Equal(t, workspace.Path, got.Path)
		assert.Equal(t, workspace.SessionName, got.SessionName)
		assert.Equal(
			t,
			tmux.ProtectedWorkspaceSocketName(
				workspace.SessionName,
				workspace.Path,
			),
			got.TmuxSocketName,
		)
		assert.Same(t, cfg, gotConfig)
		return "kwt-pr-0123456789abcdef", nil
	}
	cmd, stdout, _ := prTestCommand()

	err := runPRImport(
		cmd,
		[]string{"https://github.com/acme/widget/pull/17"},
	)

	require.NoError(t, err)
	assert.True(t, started)
	var got pullrequest.ImportResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, "kwt-pr-0123456789abcdef", got.Workspace.TmuxSocketName)
	importedWorkspace := tryRequireWorkspace(t, got.PullRequest.Workspace)
	assert.Equal(
		t,
		"kwt-pr-0123456789abcdef",
		importedWorkspace.TmuxSocketName,
	)
}

func TestRegisteredPRImportLosesToProjectRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	projectPath := filepath.Join(t.TempDir(), "widget")
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = '"+projectPath+"'\n"),
		0o600,
	))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: "github.com/acme/widget", Name: "widget", Path: projectPath,
	}}}
	serviceImpl := &fakePRService{}
	withPRCommandDeps(t, cfg, serviceImpl)
	oldBeforeAcquire := beforeProjectGuardAcquire
	t.Cleanup(func() { beforeProjectGuardAcquire = oldBeforeAcquire })
	beforeProjectGuardAcquire = func() {
		snapshot, snapshotErr := config.LoadGlobalSnapshotAt(home)
		require.NoError(t, snapshotErr)
		changed, removeErr := config.CompareAndSwapProjectAt(
			home, snapshot.Projects[0], nil,
		)
		require.NoError(t, removeErr)
		require.True(t, changed)
	}
	cmd, stdout, _ := prTestCommand()

	err := runPRImport(cmd, []string{"17"})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.Empty(t, serviceImpl.gotSelector)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
}

func tryRequireWorkspace(
	t *testing.T,
	workspace *pullrequest.Workspace,
) pullrequest.Workspace {
	t.Helper()
	require.NotNil(t, workspace)
	return *workspace
}

func TestProtectedCredentialNamesAlwaysIncludeFleetDefaults(t *testing.T) {
	for _, configured := range []string{"", "CUSTOM_FLEET_TOKEN"} {
		names := credentials.ProtectedNames(&models.Config{
			Fleet: models.FleetConfig{TokenEnv: configured},
		})
		want := []string{
			"KWT_GITHUB_TOKEN",
			"KWT_FLEET_TOKEN",
		}
		if configured != "" {
			want = append(want, configured)
		}
		assert.ElementsMatch(t, want, names)
	}
}

func TestRunPRAttachUsesPersistedWorkspaceIdentity(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	workspace := pullrequest.Workspace{
		Path:        "/worktrees/pr-32",
		Branch:      "pr-32",
		Repository:  "github.com/acme/widget",
		SessionName: "kwt-workspace-pr-32",
	}
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Path:     "/repos/widget",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-32"] = pullrequest.Provenance{
				Project:   project,
				Workspace: workspace,
			}
			return nil
		},
	))
	cfg := testPRConfig()
	cfg.Fleet.TokenEnv = "CUSTOM_FLEET_TOKEN"
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(
		t, workspace.Path, "0123456789abcdef0123456789abcdef",
	)
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	var attached bool
	attachExistingPRWorkspaceSession = func(
		_ context.Context,
		got pullrequest.Workspace,
		gotConfig *models.Config,
		_ string,
	) error {
		attached = true
		assert.Equal(t, workspace, got)
		assert.Same(t, cfg, gotConfig)
		return nil
	}
	cmd, _, _ := prTestCommand()

	err := runPRAttach(cmd, []string{workspace.Path})

	require.NoError(t, err)
	assert.True(t, attached)
}

func TestRunPRAttachPreservesLegacyGenerationForUnguardedSession(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	recorded := pullrequest.Workspace{
		Path:        "/worktrees/pr-legacy",
		Branch:      "pr-legacy",
		Repository:  "github.com/acme/widget",
		SessionName: "kwt-workspace-pr-legacy",
	}
	live := recorded
	live.Generation = "0123456789abcdef0123456789abcdef"
	project := pullrequest.Project{
		Identity: recorded.Repository,
		Path:     "/repos/widget",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records[recorded.Branch] = pullrequest.Provenance{
				Project: project, Workspace: recorded,
			}
			return nil
		},
	))
	cfg := testPRConfig()
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(t, recorded.Path, live.Generation)
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{live}, nil
	}
	ensurePRWorkspaceSession = func(
		_ context.Context,
		got pullrequest.Workspace,
		_ *models.Config,
	) (string, error) {
		if got.Generation != "" {
			return "", fmt.Errorf(
				"legacy session cannot accept generation %q",
				got.Generation,
			)
		}
		return tmux.ProtectedWorkspaceSocketName(got.SessionName, got.Path), nil
	}
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		return nil
	}
	cmd, _, _ := prTestCommand()

	err := runPRAttach(cmd, []string{recorded.Path})

	require.NoError(t, err)
}

func TestRunPRAttachGuardUsesVerifiedLiveWorkspaceIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     "/repos/widget",
	}
	recorded := pullrequest.Workspace{
		Path:        "/worktrees/pr-verified",
		Branch:      "pr-verified",
		Repository:  project.Identity,
		SessionName: "kwt-workspace-pr-verified",
	}
	live := recorded
	live.Generation = "0123456789abcdef0123456789abcdef"
	expectedSocket := tmux.ProtectedWorkspaceSocketName(
		recorded.SessionName,
		recorded.Path,
	)
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-verified"] = pullrequest.Provenance{
				Project: project, Workspace: recorded,
			}
			return nil
		},
	))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: project.Identity, Name: project.Name, Path: project.Path,
	}}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(t, recorded.Path, live.Generation)
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{live}, nil
	}
	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	fingerprint, err := snapshot.Projects[0].Fingerprint()
	require.NoError(t, err)
	prAttachExpectedRepository = project.Identity
	prAttachExpectedRegistration = fingerprint
	prAttachExpectedGeneration = live.Generation
	prAttachExpectedSession = recorded.SessionName
	prAttachExpectedSocket = expectedSocket
	cmd, _, _ := prTestCommand()
	markCommandFlagsChanged(
		t, cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
		"expected-socket",
	)
	ensurePRWorkspaceSession = func(
		_ context.Context,
		got pullrequest.Workspace,
		_ *models.Config,
	) (string, error) {
		assert.Equal(t, live.Generation, got.Generation)
		assert.Empty(t, got.TmuxSocketName)
		return expectedSocket, nil
	}
	attached := false
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		attached = true
		return nil
	}

	err = runPRAttach(cmd, []string{recorded.Path})

	require.NoError(t, err)
	assert.True(t, attached)
}

func TestRunPRAttachRejectsStaleGuardBeforeEnsuringSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     "/repos/widget",
	}
	workspace := pullrequest.Workspace{
		Path:           "/worktrees/pr-guarded",
		Branch:         "pr-guarded",
		Repository:     project.Identity,
		Generation:     "0123456789abcdef0123456789abcdef",
		SessionName:    "kwt-workspace-pr-guarded",
		TmuxSocketName: "kwt-pr-protected",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-guarded"] = pullrequest.Provenance{
				Project: project, Workspace: workspace,
			}
			return nil
		},
	))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: project.Identity, Name: project.Name, Path: project.Path,
	}}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(t, workspace.Path, workspace.Generation)
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	fingerprint, err := snapshot.Projects[0].Fingerprint()
	require.NoError(t, err)
	prAttachExpectedRepository = project.Identity
	prAttachExpectedRegistration = fingerprint
	prAttachExpectedGeneration = "fedcba9876543210fedcba9876543210"
	prAttachExpectedSession = workspace.SessionName
	prAttachExpectedSocket = workspace.TmuxSocketName
	cmd, stdout, _ := prTestCommand()
	markCommandFlagsChanged(
		t, cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
		"expected-socket",
	)
	ensured := false
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		ensured = true
		return workspace.TmuxSocketName, nil
	}

	err = runPRAttach(cmd, []string{workspace.Path})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, ensured)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
}

func TestRunPRAttachRejectsRemovedRegistrationBeforeEnsuringSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), nil, 0o600))
	workspace := pullrequest.Workspace{
		Path:        "/worktrees/pr-removed",
		Branch:      "pr-removed",
		Repository:  "github.com/acme/widget",
		SessionName: "kwt-workspace-pr-removed",
	}
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Path:     "/repos/widget",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-removed"] = pullrequest.Provenance{
				Project: project, Workspace: workspace,
			}
			return nil
		},
	))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: project.Identity, Name: "widget", Path: project.Path,
	}}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(
		t, workspace.Path, "0123456789abcdef0123456789abcdef",
	)
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	ensured := false
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		ensured = true
		return "kwt-pr-protected", nil
	}
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		return nil
	}
	cmd, stdout, _ := prTestCommand()

	err := runPRAttach(cmd, []string{workspace.Path})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, ensured)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
}

func TestRunPRAttachRejectsProvenanceReplacementWhileWaitingForProjectFence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	projectA := pullrequest.Project{
		Identity: "github.com/acme/widget-a",
		Name:     "widget-a",
		Path:     filepath.Join(t.TempDir(), "widget-a"),
	}
	projectB := pullrequest.Project{
		Identity: "github.com/acme/widget-b",
		Name:     "widget-b",
		Path:     filepath.Join(t.TempDir(), "widget-b"),
	}
	workspace := pullrequest.Workspace{
		Path:        filepath.Join(t.TempDir(), "reused"),
		Branch:      "pr-41",
		Repository:  projectA.Identity,
		SessionName: "kwt-workspace-pr-41",
	}
	recordA := pullrequest.Provenance{Project: projectA, Workspace: workspace}
	recordB := recordA
	recordB.Project = projectB
	recordB.Workspace.Repository = projectB.Identity
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["reused"] = recordA
			return nil
		},
	))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: projectA.Identity, Name: projectA.Name, Path: projectA.Path,
	}}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(
		t, workspace.Path, "0123456789abcdef0123456789abcdef",
	)
	inspectPRProjectClone = func(
		_ context.Context,
		got pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return got.Project, []pullrequest.Workspace{got.Workspace}, nil
	}
	oldBeforeAcquire := beforeProjectGuardAcquire
	t.Cleanup(func() { beforeProjectGuardAcquire = oldBeforeAcquire })
	beforeProjectGuardAcquire = func() {
		require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
			context.Background(),
			func(records map[string]pullrequest.Provenance) error {
				records["reused"] = recordB
				return nil
			},
		))
	}
	ensured := false
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		ensured = true
		return "kwt-pr-protected", nil
	}
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		return nil
	}
	cmd, stdout, _ := prTestCommand()

	err := runPRAttach(cmd, []string{workspace.Path})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, ensured)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
}

func TestRunPRAttachClassifiesDisappearanceWhileWaitingAsRegistrationChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     "/repos/widget",
	}
	workspace := pullrequest.Workspace{
		Path:        "/worktrees/pr-disappeared",
		Branch:      "pr-disappeared",
		Repository:  project.Identity,
		Generation:  "0123456789abcdef0123456789abcdef",
		SessionName: "kwt-workspace-pr-disappeared",
		TmuxSocketName: tmux.ProtectedWorkspaceSocketName(
			"kwt-workspace-pr-disappeared",
			"/worktrees/pr-disappeared",
		),
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-disappeared"] = pullrequest.Provenance{
				Project: project, Workspace: workspace,
			}
			return nil
		},
	))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: project.Identity, Name: project.Name, Path: project.Path,
	}}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	disappeared := false
	readPRWorkspaceGeneration = func(string, string) (string, error) {
		if disappeared {
			return "", fmt.Errorf(
				"read worktree identity: %w",
				gitadapter.ErrWorktreeNotFound,
			)
		}
		return workspace.Generation, nil
	}
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	oldBeforeAcquire := beforeProjectGuardAcquire
	t.Cleanup(func() { beforeProjectGuardAcquire = oldBeforeAcquire })
	beforeProjectGuardAcquire = func() { disappeared = true }
	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	fingerprint, err := snapshot.Projects[0].Fingerprint()
	require.NoError(t, err)
	prAttachExpectedRepository = project.Identity
	prAttachExpectedRegistration = fingerprint
	prAttachExpectedGeneration = workspace.Generation
	prAttachExpectedSession = workspace.SessionName
	prAttachExpectedSocket = workspace.TmuxSocketName
	cmd, stdout, _ := prTestCommand()
	markCommandFlagsChanged(
		t, cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
		"expected-socket",
	)
	ensured := false
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		ensured = true
		return workspace.TmuxSocketName, nil
	}

	err = runPRAttach(cmd, []string{workspace.Path})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, ensured)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
}

func TestRunPRAttachClassifiesDeletedWorkspaceAsRegistrationChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	repo := newPRInspectionRepo(t)
	runPRInspectionGit(
		t,
		repo,
		"remote",
		"add",
		"origin",
		"https://github.com/acme/widget.git",
	)
	branch := "pr-deleted"
	workspacePath := filepath.Join(t.TempDir(), branch)
	runPRInspectionGit(t, repo, "branch", branch)
	runPRInspectionGit(t, repo, "worktree", "add", workspacePath, branch)
	generation, err := gitadapter.New(repo).WorktreeGeneration(workspacePath)
	require.NoError(t, err)
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     repo,
	}
	workspace := pullrequest.Workspace{
		Path:        workspacePath,
		Branch:      branch,
		Repository:  project.Identity,
		Generation:  generation,
		SessionName: "kwt-workspace-pr-deleted",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records[branch] = pullrequest.Provenance{
				Project: project, Workspace: workspace,
			}
			return nil
		},
	))
	registered, err := config.RegisterProjectWithIdentity(models.Project{
		Repository: project.Identity,
		Name:       project.Name,
		Path:       project.Path,
	})
	require.NoError(t, err)
	cfg := &models.Config{Projects: []models.Project{registered.Project}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	prAttachExpectedRepository = project.Identity
	prAttachExpectedRegistration = registered.Fingerprint
	prAttachExpectedGeneration = generation
	prAttachExpectedSession = workspace.SessionName
	prAttachExpectedSocket = tmux.ProtectedWorkspaceSocketName(
		workspace.SessionName,
		workspace.Path,
	)
	cmd, stdout, _ := prTestCommand()
	markCommandFlagsChanged(
		t, cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
		"expected-socket",
	)
	require.NoError(t, os.RemoveAll(workspacePath))
	ensured := false
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		ensured = true
		return "", nil
	}

	err = runPRAttach(cmd, []string{workspacePath})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, ensured)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
}

func TestRunPRAttachClassifiesDeletedProjectCheckoutAsChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	repo := newPRInspectionRepo(t)
	branch := "pr-project-deleted"
	workspacePath := filepath.Join(t.TempDir(), branch)
	runPRInspectionGit(t, repo, "branch", branch)
	runPRInspectionGit(t, repo, "worktree", "add", workspacePath, branch)
	generation, err := gitadapter.New(repo).WorktreeGeneration(workspacePath)
	require.NoError(t, err)
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     repo,
	}
	workspace := pullrequest.Workspace{
		Path:        workspacePath,
		Branch:      branch,
		Repository:  project.Identity,
		Generation:  generation,
		SessionName: "kwt-workspace-pr-project-deleted",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records[branch] = pullrequest.Provenance{
				Project: project, Workspace: workspace,
			}
			return nil
		},
	))
	registered, err := config.RegisterProjectWithIdentity(models.Project{
		Repository: project.Identity,
		Name:       project.Name,
		Path:       project.Path,
	})
	require.NoError(t, err)
	cfg := &models.Config{Projects: []models.Project{registered.Project}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	prAttachExpectedRepository = project.Identity
	prAttachExpectedRegistration = registered.Fingerprint
	prAttachExpectedGeneration = generation
	prAttachExpectedSession = workspace.SessionName
	prAttachExpectedSocket = tmux.ProtectedWorkspaceSocketName(
		workspace.SessionName,
		workspace.Path,
	)
	cmd, stdout, _ := prTestCommand()
	markCommandFlagsChanged(
		t, cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
		"expected-socket",
	)
	require.NoError(t, os.RemoveAll(repo))
	ensured := false
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		ensured = true
		return "", nil
	}

	err = runPRAttach(cmd, []string{workspacePath})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, ensured)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
}

func TestRunPRAttachClassifiesReplacedProjectRegistrationAsChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	repo := newPRInspectionRepo(t)
	branch := "pr-replaced-registration"
	workspacePath := filepath.Join(t.TempDir(), branch)
	runPRInspectionGit(t, repo, "branch", branch)
	runPRInspectionGit(t, repo, "worktree", "add", workspacePath, branch)
	generation, err := gitadapter.New(repo).WorktreeGeneration(workspacePath)
	require.NoError(t, err)
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     repo,
	}
	workspace := pullrequest.Workspace{
		Path:        workspacePath,
		Branch:      branch,
		Repository:  project.Identity,
		Generation:  generation,
		SessionName: "kwt-workspace-pr-replaced-registration",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records[branch] = pullrequest.Provenance{
				Project: project, Workspace: workspace,
			}
			return nil
		},
	))
	expected, err := config.RegisterProjectWithIdentity(models.Project{
		Repository: project.Identity,
		Name:       project.Name,
		Path:       project.Path,
	})
	require.NoError(t, err)
	replacement, err := config.RegisterProjectWithIdentity(models.Project{
		Repository: "github.com/other/widget",
		Name:       project.Name,
		Path:       project.Path,
	})
	require.NoError(t, err)
	cfg := &models.Config{Projects: []models.Project{replacement.Project}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	validatePRProjectRoot = defaultValidatePRProjectRoot
	inspectPRProjectClone = defaultInspectPRProjectClone
	prAttachExpectedRepository = project.Identity
	prAttachExpectedRegistration = expected.Fingerprint
	prAttachExpectedGeneration = generation
	prAttachExpectedSession = workspace.SessionName
	prAttachExpectedSocket = tmux.ProtectedWorkspaceSocketName(
		workspace.SessionName,
		workspace.Path,
	)
	cmd, stdout, _ := prTestCommand()
	markCommandFlagsChanged(
		t, cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
		"expected-socket",
	)
	ensured := false
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		ensured = true
		return "", nil
	}

	err = runPRAttach(cmd, []string{workspacePath})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, ensured)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
}

func TestRunPRAttachHoldsWorktreeGenerationThroughSessionEstablishment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	repo := newPRInspectionRepo(t)
	branch := "pr-attach-removal-race"
	workspacePath := filepath.Join(t.TempDir(), branch)
	runPRInspectionGit(t, repo, "branch", branch)
	runPRInspectionGit(t, repo, "worktree", "add", workspacePath, branch)
	g := gitadapter.New(repo)
	generation, err := g.WorktreeGeneration(workspacePath)
	require.NoError(t, err)
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     repo,
	}
	workspace := pullrequest.Workspace{
		Path:        workspacePath,
		Branch:      branch,
		Repository:  project.Identity,
		Generation:  generation,
		SessionName: "kwt-workspace-pr-attach-removal-race",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records[branch] = pullrequest.Provenance{
				Project: project, Workspace: workspace,
			}
			return nil
		},
	))
	registered, err := config.RegisterProjectWithIdentity(models.Project{
		Repository: project.Identity,
		Name:       project.Name,
		Path:       project.Path,
	})
	require.NoError(t, err)
	cfg := &models.Config{Projects: []models.Project{registered.Project}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	withPRWorkspaceGeneration = defaultWithPRWorkspaceGeneration
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	prAttachExpectedRepository = project.Identity
	prAttachExpectedRegistration = registered.Fingerprint
	prAttachExpectedGeneration = generation
	prAttachExpectedSession = workspace.SessionName
	prAttachExpectedSocket = tmux.ProtectedWorkspaceSocketName(
		workspace.SessionName,
		workspace.Path,
	)
	cmd, _, _ := prTestCommand()
	markCommandFlagsChanged(
		t, cmd,
		"expected-repository",
		"expected-registration",
		"expected-generation",
		"expected-session",
		"expected-socket",
	)
	establishmentStarted := make(chan struct{})
	finishEstablishment := make(chan struct{})
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		close(establishmentStarted)
		<-finishEstablishment
		return prAttachExpectedSocket, nil
	}
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		return nil
	}
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- runPRAttach(cmd, []string{workspacePath})
	}()
	<-establishmentStarted
	mutationEntered := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- gitadapter.New(repo).WithWorktreeGeneration(
			workspacePath,
			generation,
			func() error {
				close(mutationEntered)
				return nil
			},
		)
	}()
	mutationEnteredBeforeEstablishment := false
	select {
	case <-mutationEntered:
		mutationEnteredBeforeEstablishment = true
	case <-time.After(250 * time.Millisecond):
	}
	close(finishEstablishment)
	require.NoError(t, <-attachDone)

	require.NoError(t, <-mutationDone)
	assert.False(t, mutationEnteredBeforeEstablishment)
}

func TestImportedWorkspaceProvenanceEvaluatesEachCandidateProject(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	workspacePath := "/worktrees/reused"
	currentGeneration := "0123456789abcdef0123456789abcdef"
	staleGeneration := "fedcba9876543210fedcba9876543210"
	current := pullrequest.Provenance{
		Project: pullrequest.Project{
			Identity: "github.com/acme/current",
			Path:     "/repos/current",
		},
		Workspace: pullrequest.Workspace{
			Path: workspacePath, Repository: "github.com/acme/current",
			Generation: currentGeneration, SessionName: "kwt-workspace-current",
		},
	}
	stale := pullrequest.Provenance{
		Project: pullrequest.Project{
			Identity: "github.com/acme/stale",
			Path:     "/repos/stale",
		},
		Workspace: pullrequest.Workspace{
			Path: workspacePath, Repository: "github.com/acme/stale",
			Generation: staleGeneration, SessionName: "kwt-workspace-stale",
		},
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["current"] = current
			records["stale"] = stale
			return nil
		},
	))
	oldRead := readPRWorkspaceGeneration
	oldInspect := inspectPRProjectClone
	t.Cleanup(func() {
		readPRWorkspaceGeneration = oldRead
		inspectPRProjectClone = oldInspect
	})
	inspectedProjects := map[string]bool{}
	readPRWorkspaceGeneration = func(projectPath, gotWorkspacePath string) (string, error) {
		assert.Equal(t, workspacePath, gotWorkspacePath)
		inspectedProjects[projectPath] = true
		if projectPath == stale.Project.Path {
			return "", fmt.Errorf("inspect stale candidate: %w", gitadapter.ErrWorktreeNotFound)
		}
		return currentGeneration, nil
	}
	inspectPRProjectClone = func(
		_ context.Context,
		got pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		assert.Equal(t, current, got)
		return current.Project, []pullrequest.Workspace{current.Workspace}, nil
	}

	verified, err := importedWorkspaceProvenance(
		context.Background(),
		workspacePath,
	)

	require.NoError(t, err)
	assert.Equal(t, current, verified.record)
	assert.Equal(t, currentGeneration, verified.liveGeneration)
	assert.True(t, inspectedProjects[current.Project.Path])
	assert.True(t, inspectedProjects[stale.Project.Path])
}

func TestImportedWorkspaceProvenanceDiscardsAnotherRepositoryOwner(t *testing.T) {
	testImportedWorkspaceProvenanceDiscardsAnotherRepositoryOwner(t, true)
}

func TestImportedWorkspaceProvenanceDiscardsLegacyAnotherRepositoryOwner(
	t *testing.T,
) {
	testImportedWorkspaceProvenanceDiscardsAnotherRepositoryOwner(t, false)
}

func TestImportedWorkspaceProvenanceInitializesOwnedLegacyGeneration(
	t *testing.T,
) {
	t.Setenv("KWT_HOME", t.TempDir())
	repo := newPRInspectionRepo(t)
	runPRInspectionGit(
		t,
		repo,
		"remote",
		"add",
		"origin",
		"https://github.com/acme/widget.git",
	)
	branch := "legacy-import"
	worktreePath := filepath.Join(t.TempDir(), branch)
	runPRInspectionGit(t, repo, "branch", branch)
	runPRInspectionGit(t, repo, "worktree", "add", worktreePath, branch)
	_, err := gitadapter.New(repo).ReadWorktreeGeneration(worktreePath)
	require.ErrorIs(t, err, os.ErrNotExist)

	repository := "github.com/acme/widget"
	info, ok := urlutil.CanonicalRepositoryInfo(repository)
	require.True(t, ok)
	record := pullrequest.Provenance{
		Repository: repository,
		Project: pullrequest.Project{
			Identity: repository,
			Name:     "widget",
			Path:     repo,
		},
		Workspace: pullrequest.Workspace{
			Path:        worktreePath,
			Branch:      branch,
			Repository:  repository,
			SessionName: tmux.WorkspaceSessionName(info, branch, worktreePath),
		},
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records[branch] = record
			return nil
		},
	))
	oldInspect := inspectPRProjectClone
	oldLoad := loadPRConfig
	t.Cleanup(func() {
		inspectPRProjectClone = oldInspect
		loadPRConfig = oldLoad
	})
	inspectPRProjectClone = defaultInspectPRProjectClone
	loadPRConfig = func() (*models.Config, error) {
		return &models.Config{Projects: []models.Project{{
			Repository: repository,
			Name:       record.Project.Name,
			Path:       repo,
		}}}, nil
	}

	verified, err := importedWorkspaceProvenance(
		context.Background(),
		worktreePath,
	)

	require.NoError(t, err)
	assert.Equal(t, record, verified.record)
	require.NoError(t, gitadapter.ValidateWorktreeGeneration(
		verified.liveGeneration,
	))
	persistedGeneration, err := gitadapter.New(repo).ReadWorktreeGeneration(
		worktreePath,
	)
	require.NoError(t, err)
	assert.Equal(t, verified.liveGeneration, persistedGeneration)
}

func testImportedWorkspaceProvenanceDiscardsAnotherRepositoryOwner(
	t *testing.T,
	staleHasGeneration bool,
) {
	t.Helper()
	t.Setenv("KWT_HOME", t.TempDir())
	staleRepo := newPRInspectionRepo(t)
	currentRepo := newPRInspectionRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "reused-worktree")
	runPRInspectionGit(t, staleRepo, "branch", "stale-owner")
	runPRInspectionGit(
		t, staleRepo, "worktree", "add", worktreePath, "stale-owner",
	)
	staleGeneration, err := gitadapter.New(staleRepo).WorktreeGeneration(
		worktreePath,
	)
	require.NoError(t, err)
	runPRInspectionGit(
		t, staleRepo, "worktree", "remove", "--force", worktreePath,
	)
	runPRInspectionGit(t, currentRepo, "branch", "current-owner")
	runPRInspectionGit(
		t, currentRepo, "worktree", "add", worktreePath, "current-owner",
	)
	currentGeneration, err := gitadapter.New(currentRepo).WorktreeGeneration(
		worktreePath,
	)
	require.NoError(t, err)
	require.NotEqual(t, staleGeneration, currentGeneration)
	staleRecordGeneration := staleGeneration
	if !staleHasGeneration {
		staleRecordGeneration = ""
	}
	stale := pullrequest.Provenance{
		Project: pullrequest.Project{
			Identity: "github.com/acme/stale", Path: staleRepo,
		},
		Workspace: pullrequest.Workspace{
			Path: worktreePath, Repository: "github.com/acme/stale",
			Generation:  staleRecordGeneration,
			SessionName: "kwt-workspace-stale",
		},
	}
	current := pullrequest.Provenance{
		Project: pullrequest.Project{
			Identity: "github.com/acme/current", Path: currentRepo,
		},
		Workspace: pullrequest.Workspace{
			Path: worktreePath, Repository: "github.com/acme/current",
			Generation: currentGeneration, SessionName: "kwt-workspace-current",
		},
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["stale"] = stale
			records["current"] = current
			return nil
		},
	))
	oldInspect := inspectPRProjectClone
	t.Cleanup(func() { inspectPRProjectClone = oldInspect })
	inspectPRProjectClone = func(
		_ context.Context,
		got pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		assert.Equal(t, current, got)
		return current.Project, []pullrequest.Workspace{current.Workspace}, nil
	}

	verified, err := importedWorkspaceProvenance(
		context.Background(),
		worktreePath,
	)

	require.NoError(t, err)
	assert.Equal(t, current, verified.record)
	assert.Equal(t, currentGeneration, verified.liveGeneration)
}

func TestProtectedAttachReleasesFenceBeforeBlockingClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	projectPath := filepath.Join(t.TempDir(), "widget")
	workspacePath := filepath.Join(t.TempDir(), "pr-34")
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     projectPath,
	}
	workspace := pullrequest.Workspace{
		Path:        workspacePath,
		Branch:      "pr-34",
		Repository:  project.Identity,
		SessionName: "kwt-workspace-pr-34",
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = '"+projectPath+"'\n"),
		0o600,
	))
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-34"] = pullrequest.Provenance{
				Project: project, Workspace: workspace,
			}
			return nil
		},
	))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: project.Identity, Name: project.Name, Path: project.Path,
	}}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(
		t, workspace.Path, "0123456789abcdef0123456789abcdef",
	)
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		return "kwt-pr-protected", nil
	}
	attachStarted := make(chan struct{})
	releaseAttach := make(chan struct{})
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		close(attachStarted)
		<-releaseAttach
		return nil
	}
	cmd, _, _ := prTestCommand()
	done := make(chan error, 1)
	go func() { done <- runPRAttach(cmd, []string{workspacePath}) }()
	<-attachStarted
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)
	claim, err := lifecycle.ObserveProjectClaim(
		context.Background(), home, projectPath, expansion,
	)
	require.NoError(t, err)
	releaseFence, err := lifecycle.AcquireProjectClaim(
		context.Background(), home, claim,
	)
	require.NoError(t, err)
	require.NoError(t, releaseFence())
	close(releaseAttach)

	require.NoError(t, <-done)
}

func TestProtectedAttachEstablishesSessionBeforeRemovalProbes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	projectPath := filepath.Join(t.TempDir(), "widget")
	workspacePath := filepath.Join(t.TempDir(), "pr-guarded")
	project := pullrequest.Project{
		Identity: "github.com/acme/widget", Name: "widget", Path: projectPath,
	}
	workspace := pullrequest.Workspace{
		Path: workspacePath, Branch: "pr-guarded",
		Repository: project.Identity, SessionName: "kwt-workspace-pr-guarded",
		Generation: "0123456789abcdef0123456789abcdef",
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = '"+projectPath+"'\n"),
		0o600,
	))
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-guarded"] = pullrequest.Provenance{
				Repository: project.Identity,
				Project:    project, Workspace: workspace,
			}
			return nil
		},
	))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: project.Identity, Name: project.Name, Path: project.Path,
	}}}
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(t, workspace.Path, workspace.Generation)
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	socketName := tmux.ProtectedWorkspaceSocketName(
		workspace.SessionName, workspace.Path,
	)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socketName, "kill-server").Run()
	})
	established := make(chan struct{})
	finishEnsure := make(chan struct{})
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		command := exec.Command(
			"tmux", "-L", socketName, "new-session", "-d",
			"-s", workspace.SessionName, "sleep", "60",
		)
		if output, runErr := command.CombinedOutput(); runErr != nil {
			return "", fmt.Errorf("start tmux: %w: %s", runErr, output)
		}
		close(established)
		<-finishEnsure
		return socketName, nil
	}
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		return nil
	}
	cmd, _, _ := prTestCommand()
	attachDone := make(chan error, 1)
	go func() { attachDone <- runPRAttach(cmd, []string{workspace.Path}) }()
	<-established
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)
	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	fingerprint, err := snapshot.Projects[0].Fingerprint()
	require.NoError(t, err)
	remover := kwt.NewProjectRemovalService(
		kwt.ProjectRemovalServiceOptions{Home: home},
	)
	removeDone := make(chan error, 1)
	go func() {
		_, removeErr := remover.RemoveProject(
			context.Background(),
			kwt.ProjectRemovalRequest{
				Path: projectPath, ExpectedRepository: project.Identity,
				ExpectedRegistration: fingerprint, Expansion: expansion,
			},
		)
		removeDone <- removeErr
	}()
	select {
	case err := <-removeDone:
		t.Fatalf("removal completed while session establishment held the claim: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishEnsure)
	require.NoError(t, <-attachDone)

	err = <-removeDone

	assert.True(t, service.IsCode(err, service.ProtectedSessionLive))
}

func TestRunPRAttachUsesTransferredProvenanceAliasHistory(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	registeredIdentity := "github.com/legacy/widget"
	resolvedIdentity := "github.com/current/widget"
	workspacePath := "/worktrees/pr-33"
	branch := "pr-33"
	resolvedInfo, ok := urlutil.CanonicalRepositoryInfo(resolvedIdentity)
	require.True(t, ok)
	registeredInfo, ok := urlutil.CanonicalRepositoryInfo(registeredIdentity)
	require.True(t, ok)
	record := pullrequest.Provenance{
		PullRequestID: "github:github.com/current/widget#33",
		Repository:    resolvedIdentity,
		RepositoryAliases: []string{
			registeredIdentity,
			"github.com/middle/widget",
			resolvedIdentity,
		},
		Project: pullrequest.Project{
			Identity: resolvedIdentity,
			Path:     "/repos/widget",
		},
		Workspace: pullrequest.Workspace{
			Path:       workspacePath,
			Branch:     branch,
			Repository: resolvedIdentity,
			SessionName: tmux.WorkspaceSessionName(
				resolvedInfo,
				branch,
				workspacePath,
			),
		},
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records[record.PullRequestID] = record
			return nil
		},
	))
	cfg := testPRConfig()
	cfg.Projects[0].Repository = registeredIdentity
	withPRCommandDeps(t, cfg, &fakePRService{})
	stubPRWorkspaceGeneration(
		t, workspacePath, "0123456789abcdef0123456789abcdef",
	)
	inspectPRProjectClone = func(
		_ context.Context,
		got pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		assert.Equal(t, record, got)
		return pullrequest.Project{
				Identity: registeredIdentity,
				Path:     record.Project.Path,
			}, []pullrequest.Workspace{{
				Path:       workspacePath,
				Branch:     branch,
				Repository: registeredIdentity,
				SessionName: tmux.WorkspaceSessionName(
					registeredInfo,
					branch,
					workspacePath,
				),
			}}, nil
	}
	attached := false
	attachExistingPRWorkspaceSession = func(
		_ context.Context,
		got pullrequest.Workspace,
		gotConfig *models.Config,
		_ string,
	) error {
		attached = true
		assert.Equal(t, record.Workspace, got)
		assert.Same(t, cfg, gotConfig)
		return nil
	}
	cmd, _, _ := prTestCommand()

	err := runPRAttach(cmd, []string{workspacePath})

	require.NoError(t, err)
	assert.True(t, attached)
}

func TestRunPRAttachRejectsStaleProvenanceAgainstLiveInventory(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	recorded := pullrequest.Workspace{
		Path:        "/worktrees/reused",
		Branch:      "pr-32",
		Repository:  "github.com/acme/widget",
		SessionName: "kwt-workspace-pr-32",
	}
	project := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Path:     "/repos/widget",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-32"] = pullrequest.Provenance{
				Project: project, Workspace: recorded,
			}
			return nil
		},
	))
	withPRCommandDeps(t, testPRConfig(), &fakePRService{})
	stubPRWorkspaceGeneration(
		t, recorded.Path, "0123456789abcdef0123456789abcdef",
	)
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		live := recorded
		live.Branch = "unrelated"
		return project, []pullrequest.Workspace{live}, nil
	}
	attached := false
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		attached = true
		return nil
	}
	cmd, _, _ := prTestCommand()

	err := runPRAttach(cmd, []string{recorded.Path})

	assertPRCode(t, err, pullrequest.CodeWorkspaceCreation)
	assert.False(t, attached)
}

func TestRunPRAttachIgnoresStaleGenerationBeforeCounting(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	live := pullrequest.Workspace{
		Path: "/worktrees/reused", Branch: "pr-32",
		Repository: "github.com/acme/widget", SessionName: "kwt-workspace-pr-32",
		Generation: "0123456789abcdef0123456789abcdef",
	}
	project := pullrequest.Project{Identity: live.Repository, Path: "/repos/widget"}
	stale := live
	stale.Generation = "fedcba9876543210fedcba9876543210"
	record := pullrequest.Provenance{Project: project, Workspace: live}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["current"] = record
			records["stale"] = pullrequest.Provenance{Project: project, Workspace: stale}
			return nil
		},
	))
	withPRCommandDeps(t, testPRConfig(), &fakePRService{})
	readPRWorkspaceGeneration = func(string, string) (string, error) {
		return live.Generation, nil
	}
	inspectPRProjectClone = func(
		_ context.Context,
		got pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		assert.Equal(t, record, got)
		return project, []pullrequest.Workspace{live}, nil
	}
	attached := false
	attachExistingPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
		string,
	) error {
		attached = true
		return nil
	}
	cmd, _, _ := prTestCommand()

	err := runPRAttach(cmd, []string{live.Path})

	require.NoError(t, err)
	assert.True(t, attached)
}

func TestImportedWorkspaceProvenanceRejectsUnreadableLiveGeneration(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	workspace := pullrequest.Workspace{
		Path: "/worktrees/pr-32", Branch: "pr-32",
		Generation: "0123456789abcdef0123456789abcdef",
	}
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["record"] = pullrequest.Provenance{Workspace: workspace}
			return nil
		},
	))
	oldRead := readPRWorkspaceGeneration
	t.Cleanup(func() { readPRWorkspaceGeneration = oldRead })
	readPRWorkspaceGeneration = func(string, string) (string, error) {
		return "", errors.New("generation unavailable")
	}

	_, err := importedWorkspaceProvenance(context.Background(), workspace.Path)

	assertPRCode(t, err, pullrequest.CodeWorkspaceCreation)
	assert.Contains(t, err.Error(), "generation")
}

func TestProtectedWorkspaceOpenMatchesGeneration(t *testing.T) {
	tests := []struct {
		name               string
		recordedGeneration string
		liveGeneration     string
		wantProtected      bool
	}{
		{
			name: "matching", recordedGeneration: "0123456789abcdef0123456789abcdef",
			liveGeneration: "0123456789abcdef0123456789abcdef",
			wantProtected:  true,
		},
		{
			name: "replacement", recordedGeneration: "fedcba9876543210fedcba9876543210",
			liveGeneration: "0123456789abcdef0123456789abcdef",
			wantProtected:  false,
		},
		{
			name:           "legacy",
			liveGeneration: "0123456789abcdef0123456789abcdef", wantProtected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KWT_HOME", t.TempDir())
			path := "/worktrees/reused"
			require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
				context.Background(),
				func(records map[string]pullrequest.Provenance) error {
					records["record"] = pullrequest.Provenance{Workspace: pullrequest.Workspace{
						Path: path, Generation: tt.recordedGeneration,
					}}
					return nil
				},
			))
			oldRead := readPRWorkspaceGeneration
			t.Cleanup(func() { readPRWorkspaceGeneration = oldRead })
			readCalls := 0
			readPRWorkspaceGeneration = func(_, gotPath string) (string, error) {
				readCalls++
				assert.Equal(t, path, gotPath)
				return tt.liveGeneration, nil
			}

			err := rejectProtectedWorkspaceOpen(context.Background(), path, "")

			if tt.wantProtected {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, 1, readCalls)
		})
	}
}

func TestProtectedWorkspaceOpenFailsClosedWhenLiveGenerationIsUnavailable(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	path := "/worktrees/protected"
	recordedGeneration := "0123456789abcdef0123456789abcdef"
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["record"] = pullrequest.Provenance{Workspace: pullrequest.Workspace{
				Path: path, Generation: recordedGeneration,
			}}
			return nil
		},
	))
	oldRead := readPRWorkspaceGeneration
	t.Cleanup(func() { readPRWorkspaceGeneration = oldRead })
	readPRWorkspaceGeneration = func(string, string) (string, error) {
		return "", errors.New("generation unavailable")
	}

	err := rejectProtectedWorkspaceOpen(context.Background(), path, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "live generation")
}

func TestProtectedWorkspaceOpenUsesPlatformPathIdentity(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path identity is case-insensitive")
	}
	t.Setenv("KWT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "protected-workspace")
	recordedPath := strings.ToUpper(path)
	generation := "0123456789abcdef0123456789abcdef"
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["record"] = pullrequest.Provenance{Workspace: pullrequest.Workspace{
				Path: recordedPath, Generation: generation,
			}}
			return nil
		},
	))
	oldRead := readPRWorkspaceGeneration
	t.Cleanup(func() { readPRWorkspaceGeneration = oldRead })
	readPRWorkspaceGeneration = func(_, gotPath string) (string, error) {
		assert.Equal(t, path, gotPath)
		return generation, nil
	}

	err := rejectProtectedWorkspaceOpen(context.Background(), path, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "protected pull-request workspace")
}

func TestProtectedWorkspaceOpenRecognizesMovedWorkspaceByGeneration(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	originalPath := "/worktrees/original"
	movedPath := "/worktrees/moved"
	generation := "0123456789abcdef0123456789abcdef"
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["record"] = pullrequest.Provenance{Workspace: pullrequest.Workspace{
				Path: originalPath, Generation: generation,
			}}
			return nil
		},
	))
	oldRead := readPRWorkspaceGeneration
	t.Cleanup(func() { readPRWorkspaceGeneration = oldRead })
	readPRWorkspaceGeneration = func(_, gotPath string) (string, error) {
		return "", fmt.Errorf("unexpected generation read for %s", gotPath)
	}

	err := rejectProtectedWorkspaceOpen(context.Background(), movedPath, generation)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kwt pr attach")
}

func TestProtectedWorkspaceOpenSkipsGenerationReadWithoutProvenance(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	oldRead := readPRWorkspaceGeneration
	t.Cleanup(func() { readPRWorkspaceGeneration = oldRead })
	readCalls := 0
	readPRWorkspaceGeneration = func(string, string) (string, error) {
		readCalls++
		return "", errors.New("must not be called")
	}

	err := rejectProtectedWorkspaceOpen(
		context.Background(),
		"/worktrees/ordinary",
		"",
	)

	require.NoError(t, err)
	assert.Zero(t, readCalls)
}

func TestInspectPRProjectCloneUsesRegisteredIdentityOverForkOrigin(
	t *testing.T,
) {
	repo := newPRInspectionRepo(t)
	runPRInspectionGit(
		t,
		repo,
		"remote",
		"add",
		"origin",
		"https://github.com/contributor/widget.git",
	)
	recorded := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     repo,
	}
	oldLoad := loadPRConfig
	t.Cleanup(func() { loadPRConfig = oldLoad })
	loadPRConfig = func() (*models.Config, error) {
		return &models.Config{Projects: []models.Project{{
			Repository: recorded.Identity,
			Name:       recorded.Name,
			Path:       recorded.Path,
		}}}, nil
	}

	project, workspaces, err := defaultInspectPRProjectClone(
		context.Background(),
		pullrequest.Provenance{
			Repository: recorded.Identity,
			Project:    recorded,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, recorded.Identity, project.Identity)
	require.NotEmpty(t, workspaces)
	assert.Equal(t, recorded.Identity, workspaces[0].Repository)
}

func TestInspectPRProjectCloneAcceptsTransferredRegisteredAlias(
	t *testing.T,
) {
	repo := newPRInspectionRepo(t)
	runPRInspectionGit(
		t,
		repo,
		"remote",
		"add",
		"origin",
		"https://github.com/current/widget.git",
	)
	registeredIdentity := "github.com/legacy/widget"
	recorded := pullrequest.Provenance{
		Repository: "github.com/current/widget",
		RepositoryAliases: []string{
			registeredIdentity,
			"github.com/middle/widget",
			"github.com/current/widget",
		},
		Project: pullrequest.Project{
			Identity: "github.com/current/widget",
			Name:     "widget",
			Path:     repo,
		},
	}
	oldLoad := loadPRConfig
	t.Cleanup(func() { loadPRConfig = oldLoad })
	loadPRConfig = func() (*models.Config, error) {
		return &models.Config{Projects: []models.Project{{
			Repository: registeredIdentity,
			Name:       recorded.Project.Name,
			Path:       recorded.Project.Path,
		}}}, nil
	}

	project, workspaces, err := defaultInspectPRProjectClone(
		context.Background(),
		recorded,
	)

	require.NoError(t, err)
	assert.Equal(t, registeredIdentity, project.Identity)
	require.NotEmpty(t, workspaces)
	assert.Equal(t, registeredIdentity, workspaces[0].Repository)
}

func TestTransferredProvenanceMatchesLiveWorkspaceAcrossAliases(
	t *testing.T,
) {
	path := "/worktrees/pr-32"
	branch := "pr-32"
	liveInfo, ok := urlutil.CanonicalRepositoryInfo(
		"github.com/legacy/widget",
	)
	require.True(t, ok)
	record := pullrequest.Provenance{
		Repository: "github.com/current/widget",
		RepositoryAliases: []string{
			"github.com/legacy/widget",
			"github.com/middle/widget",
			"github.com/current/widget",
		},
		Project: pullrequest.Project{
			Identity: "github.com/current/widget",
			Path:     "/repos/widget",
		},
		Workspace: pullrequest.Workspace{
			Repository: "github.com/current/widget",
			Branch:     branch,
			Path:       path,
			SessionName: "kwt-workspace-github-com-current-widget-pr-32-" +
				template.ShortHash(path),
		},
	}
	liveProject := pullrequest.Project{
		Identity: "github.com/legacy/widget",
		Path:     record.Project.Path,
	}
	liveWorkspace := pullrequest.Workspace{
		Repository: liveProject.Identity,
		Branch:     branch,
		Path:       path,
		SessionName: tmux.WorkspaceSessionName(
			liveInfo,
			branch,
			path,
		),
	}

	assert.True(t, samePRProjectClone(record, liveProject))
	assert.True(t, containsPRWorkspace([]pullrequest.Workspace{
		liveWorkspace,
	}, record))

	record.Workspace.SessionName = "tampered"
	assert.False(t, containsPRWorkspace([]pullrequest.Workspace{
		liveWorkspace,
	}, record))
}

func TestContainsPRWorkspaceRejectsMismatchedGeneration(t *testing.T) {
	live := pullrequest.Workspace{
		Path: "/worktrees/reused", Branch: "pr-32",
		Repository: "github.com/acme/widget", SessionName: "kwt-workspace-pr-32",
		Generation: "0123456789abcdef0123456789abcdef",
	}
	record := pullrequest.Provenance{
		Project: pullrequest.Project{Identity: live.Repository},
		Workspace: pullrequest.Workspace{
			Path: live.Path, Branch: live.Branch,
			Repository: live.Repository, SessionName: live.SessionName,
			Generation: "fedcba9876543210fedcba9876543210",
		},
	}

	assert.False(t, containsPRWorkspace([]pullrequest.Workspace{live}, record))
	record.Workspace.Generation = ""
	assert.True(t, containsPRWorkspace([]pullrequest.Workspace{live}, record))
}

func TestInspectPRProjectCloneUsesLiveIdentityWithoutRegistration(
	t *testing.T,
) {
	repo := newPRInspectionRepo(t)
	runPRInspectionGit(
		t,
		repo,
		"remote",
		"add",
		"origin",
		"https://github.com/acme/widget.git",
	)
	recorded := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     repo,
	}
	oldLoad := loadPRConfig
	t.Cleanup(func() { loadPRConfig = oldLoad })
	loadPRConfig = func() (*models.Config, error) {
		return &models.Config{}, nil
	}

	project, workspaces, err := defaultInspectPRProjectClone(
		context.Background(),
		pullrequest.Provenance{
			Repository: recorded.Identity,
			Project:    recorded,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, recorded.Identity, project.Identity)
	require.NotEmpty(t, workspaces)
	assert.Equal(t, recorded.Identity, workspaces[0].Repository)
}

func TestInspectPRProjectCloneRejectsConflictingRegistration(
	t *testing.T,
) {
	repo := newPRInspectionRepo(t)
	recorded := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     repo,
	}
	oldLoad := loadPRConfig
	t.Cleanup(func() { loadPRConfig = oldLoad })
	loadPRConfig = func() (*models.Config, error) {
		return &models.Config{Projects: []models.Project{{
			Repository: "github.com/other/widget",
			Name:       recorded.Name,
			Path:       recorded.Path,
		}}}, nil
	}

	_, _, err := defaultInspectPRProjectClone(
		context.Background(),
		pullrequest.Provenance{
			Repository: recorded.Identity,
			Project:    recorded,
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicts with recorded identity")
}

func TestInspectPRProjectCloneRejectsAmbiguousRegistrations(
	t *testing.T,
) {
	repo := newPRInspectionRepo(t)
	recorded := pullrequest.Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     repo,
	}
	oldLoad := loadPRConfig
	t.Cleanup(func() { loadPRConfig = oldLoad })
	loadPRConfig = func() (*models.Config, error) {
		return &models.Config{Projects: []models.Project{
			{
				Repository: recorded.Identity,
				Name:       recorded.Name,
				Path:       recorded.Path,
			},
			{
				Repository: recorded.Identity,
				Name:       "widget-copy",
				Path:       recorded.Path,
			},
		}}, nil
	}

	_, _, err := defaultInspectPRProjectClone(
		context.Background(),
		pullrequest.Provenance{
			Repository: recorded.Identity,
			Project:    recorded,
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not uniquely registered")
}

func TestLivePRWorkspacesExcludePrunableAndMissingPaths(
	t *testing.T,
) {
	livePath := t.TempDir()
	require.NoError(t, os.Mkdir(
		filepath.Join(livePath, ".git"),
		0o755,
	))
	info, ok := urlutil.CanonicalRepositoryInfo("github.com/acme/widget")
	require.True(t, ok)

	workspaces := livePRWorkspaces(
		info,
		pullrequest.Project{Identity: info.FullPath},
		[]models.Worktree{
			{
				Path:     livePath,
				Branch:   "prunable",
				Prunable: true,
			},
			{
				Path:   filepath.Join(t.TempDir(), "missing"),
				Branch: "missing",
			},
			{
				Path:       livePath,
				Branch:     "live",
				Generation: "0123456789abcdef0123456789abcdef",
			},
		},
	)

	require.Len(t, workspaces, 1)
	assert.Equal(t, "live", workspaces[0].Branch)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", workspaces[0].Generation)
}

func newPRInspectionRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	runPRInspectionGit(t, "", "init", "-b", "main", repo)
	runPRInspectionGit(t, repo, "config", "user.name", "Test User")
	runPRInspectionGit(t, repo, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "README.md"),
		[]byte("# widget\n"),
		0o644,
	))
	runPRInspectionGit(t, repo, "add", "README.md")
	runPRInspectionGit(t, repo, "commit", "-m", "Initial commit")
	return repo
}

func runPRInspectionGit(
	t *testing.T,
	dir string,
	args ...string,
) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestRunPRImportPreflightsSessionBeforeMutation(t *testing.T) {
	service := &fakePRService{}
	withPRCommandDeps(t, testPRConfig(), service)
	prStartSession = true
	validatePRWorkspaceSessionConfig = func(*models.Config) error {
		return errors.New("invalid layout")
	}
	cmd, stdout, _ := prTestCommand()

	err := runPRImport(cmd, []string{"17"})

	assert.Empty(t, service.gotSelector)
	var typed *pullrequest.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, pullrequest.CodeWorkspaceCreation, typed.Code)
	var envelope pullrequest.ErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	require.NotNil(t, envelope.Error)
	assert.Equal(t, pullrequest.CodeWorkspaceCreation, envelope.Error.Code)
}

func TestRunPRImportReportsDurableImportWithSessionFailure(t *testing.T) {
	service := &fakePRService{result: pullrequest.ImportResult{
		Status: pullrequest.ImportCreated,
		Workspace: pullrequest.Workspace{
			ID: "ws", Path: "/worktrees/ws",
			SessionName: "kwt-workspace-ws",
		},
	}}
	withPRCommandDeps(t, testPRConfig(), service)
	prStartSession = true
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		return "", errors.New("invalid layout")
	}
	cmd, stdout, _ := prTestCommand()

	err := runPRImport(cmd, []string{"17"})

	require.NoError(t, err)
	var result pullrequest.ImportResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	assert.Equal(t, pullrequest.ImportCreated, result.Status)
	require.NotNil(t, result.SessionStartError)
	assert.Equal(
		t,
		pullrequest.CodeWorkspaceCreation,
		result.SessionStartError.Code,
	)
	assert.False(t, result.SessionStartError.Retryable)
}

func TestRunPRImportReportsSessionSafetyFailure(t *testing.T) {
	service := &fakePRService{result: pullrequest.ImportResult{
		Status: pullrequest.ImportExisting,
		Workspace: pullrequest.Workspace{
			ID: "ws", Path: "/worktrees/ws",
			SessionName: "kwt-workspace-ws",
		},
	}}
	withPRCommandDeps(t, testPRConfig(), service)
	prStartSession = true
	ensurePRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) (string, error) {
		return "", &tmux.SessionSafetyError{
			Reason: "existing tmux session is not verified",
		}
	}
	cmd, stdout, _ := prTestCommand()

	err := runPRImport(cmd, []string{"17"})

	require.NoError(t, err)
	var result pullrequest.ImportResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	require.NotNil(t, result.SessionStartError)
	assert.Equal(
		t,
		"existing tmux session is not verified",
		result.SessionStartError.Message,
	)
}

func TestPRCommandWritesTypedJSONErrorAndExitStatus(t *testing.T) {
	service := &fakePRService{listErr: pullrequest.NewError(
		pullrequest.CodeAuthentication, "GitHub authentication failed", false, errors.New("secret cause"))}
	withPRCommandDeps(t, testPRConfig(), service)
	cmd, stdout, stderr := prTestCommand()

	err := runPRList(cmd, nil)

	var exitErr *prCommandError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 3, exitErr.ExitCode())
	var envelope pullrequest.ErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	require.NotNil(t, envelope.Error)
	assert.Equal(t, pullrequest.CodeAuthentication, envelope.Error.Code)
	assert.NotContains(t, stdout.String(), "secret cause")
	assert.Contains(t, stderr.String(), "authentication_failed")
}

func TestPRFailureCategoriesHaveDistinctExitStatuses(t *testing.T) {
	codes := []pullrequest.ErrorCode{
		pullrequest.CodeAuthentication,
		pullrequest.CodeRepositoryMismatch,
		pullrequest.CodeNotFound,
		pullrequest.CodeInaccessibleHead,
		pullrequest.CodeNamingConflict,
		pullrequest.CodeNetwork,
		pullrequest.CodeWorkspaceCreation,
		pullrequest.CodeMalformedResponse,
		pullrequest.CodeConflict,
		pullrequest.CodeUnsupportedGitVersion,
	}
	seen := make(map[int]pullrequest.ErrorCode)
	for _, code := range codes {
		exit := prExitCode(code)
		if previous, ok := seen[exit]; ok {
			t.Fatalf("%s and %s share exit status %d", previous, code, exit)
		}
		seen[exit] = code
	}
}

func TestResolvePRProjectSupportsStableIdentityNameAndPath(t *testing.T) {
	otherPath, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	widgetPath, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cfg := &models.Config{Projects: []models.Project{
		{Repository: "github.com/acme/other", Name: "other", Path: otherPath},
		{Repository: "github.com/acme/widget", Name: "widget", Path: widgetPath},
	}}
	for _, selector := range []string{"github.com/acme/widget", "widget", widgetPath} {
		t.Run(selector, func(t *testing.T) {
			project, err := resolvePRProject(cfg, selector)
			require.NoError(t, err)
			assert.Equal(t, "github.com/acme/widget", project.Identity)
			assert.Equal(t, widgetPath, project.Path)
		})
	}
}

func TestResolvePRProjectRejectsAmbiguousProjectName(t *testing.T) {
	acmePath, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	octocatPath, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cfg := &models.Config{Projects: []models.Project{
		{Repository: "github.com/acme/widget", Name: "widget", Path: acmePath},
		{Repository: "github.com/octocat/widget", Name: "Widget", Path: octocatPath},
	}}

	_, err = resolvePRProject(cfg, "widget")

	assertPRCode(t, err, pullrequest.CodeRepositoryMismatch)
	assert.Contains(t, err.Error(), "ambiguous")

	for _, selector := range []string{"github.com/octocat/widget", octocatPath} {
		project, selectErr := resolvePRProject(cfg, selector)
		require.NoError(t, selectErr)
		assert.Equal(t, "github.com/octocat/widget", project.Identity)
	}
}

func TestResolvePRProjectPrefersIdentityAndNameOverCallerRelativePaths(t *testing.T) {
	caller := t.TempDir()
	changeDir(t, caller)
	identityCollision := filepath.Join(caller, "github.com", "acme", "widget")
	nameCollision := filepath.Join(caller, "widget")
	require.NoError(t, os.MkdirAll(identityCollision, 0o755))
	require.NoError(t, os.MkdirAll(nameCollision, 0o755))
	desiredPath := t.TempDir()
	cfg := &models.Config{Projects: []models.Project{
		{Repository: "github.com/attacker/identity-collision", Name: "identity-collision", Path: identityCollision},
		{Repository: "github.com/attacker/name-collision", Name: "name-collision", Path: nameCollision},
		{Repository: "github.com/acme/widget", Name: "widget", Path: desiredPath},
	}}

	for _, selector := range []string{"github.com/acme/widget", "widget"} {
		project, err := resolvePRProject(cfg, selector)

		require.NoError(t, err)
		assert.Equal(t, "github.com/acme/widget", project.Identity)
		assert.Equal(t, desiredPath, project.Path)
	}
}

func TestResolvePRProjectRejectsRelativeAndSymlinkPathSelectors(t *testing.T) {
	caller := t.TempDir()
	changeDir(t, caller)
	projectPath := filepath.Join(caller, "repos", "widget")
	require.NoError(t, os.MkdirAll(projectPath, 0o755))
	cfg := &models.Config{Projects: []models.Project{{
		Repository: "github.com/acme/widget", Name: "widget", Path: projectPath,
	}}}

	_, err := resolvePRProject(cfg, filepath.Join("repos", "widget"))
	assertPRCode(t, err, pullrequest.CodeRepositoryMismatch)

	symlinkPath := filepath.Join(caller, "widget-link")
	if symlinkErr := os.Symlink(projectPath, symlinkPath); symlinkErr != nil {
		t.Skipf("symlinks unavailable: %v", symlinkErr)
	}
	_, err = resolvePRProject(cfg, symlinkPath)
	assertPRCode(t, err, pullrequest.CodeRepositoryMismatch)
}

func TestValidatePRProjectNormalizesGitHubIdentityCase(t *testing.T) {
	project, err := validatePRProject(pullrequest.Project{
		Identity: "GitHub.com/Acme/Widget", Name: "widget", Path: "/repos/widget",
	})

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", project.Identity)
}

func TestSamePRPathUsesPlatformPathIdentity(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path identity is case-insensitive")
	}
	root := t.TempDir()
	assert.True(t, samePRPath(root, strings.ToUpper(root)))
}

func TestValidatePRProjectRejectsEmptyPath(t *testing.T) {
	_, err := validatePRProject(pullrequest.Project{
		Identity: "github.com/acme/widget", Name: "widget",
	})

	assertPRCode(t, err, pullrequest.CodeRepositoryMismatch)
}

func TestDefaultValidatePRProjectRootRejectsCallerRelativePath(t *testing.T) {
	_, err := defaultValidatePRProjectRoot(pullrequest.Project{
		Identity: "github.com/acme/widget", Name: "widget", Path: ".",
	})

	assertPRCode(t, err, pullrequest.CodeRepositoryMismatch)
}

func TestResolvePRProjectRejectsMismatchAndUnsupportedProvider(t *testing.T) {
	cfg := testPRConfig()
	_, err := resolvePRProject(cfg, "missing")
	assertPRCode(t, err, pullrequest.CodeRepositoryMismatch)

	cfg.Projects[0].Repository = "gitlab.com/acme/widget"
	_, err = resolvePRProject(cfg, "widget")
	assertPRCode(t, err, pullrequest.CodeUnsupportedProvider)
}

func assertPRCode(t *testing.T, err error, code pullrequest.ErrorCode) {
	t.Helper()
	var typed *pullrequest.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, code, typed.Code)
}
