package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-github/v89/github"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	urlutil "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/pkg/models"
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
	oldLoad := loadPRConfig
	oldTargetLoad := loadPRTargetConfig
	oldNew := newPRService
	oldProvider := newPRGitHubProvider
	oldValidateRoot := validatePRProjectRoot
	oldProject := prProject
	oldState := prState
	oldStartSession := prStartSession
	oldValidateSessionConfig := validatePRWorkspaceSessionConfig
	oldStartWorkspaceSession := startPRWorkspaceSession
	oldAttachWorkspaceSession := attachPRWorkspaceSession
	oldInspectProjectClone := inspectPRProjectClone
	t.Cleanup(func() {
		loadPRConfig = oldLoad
		loadPRTargetConfig = oldTargetLoad
		newPRService = oldNew
		newPRGitHubProvider = oldProvider
		validatePRProjectRoot = oldValidateRoot
		prProject = oldProject
		prState = oldState
		prStartSession = oldStartSession
		validatePRWorkspaceSessionConfig = oldValidateSessionConfig
		startPRWorkspaceSession = oldStartWorkspaceSession
		attachPRWorkspaceSession = oldAttachWorkspaceSession
		inspectPRProjectClone = oldInspectProjectClone
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
	validatePRWorkspaceSessionConfig = func(*models.Config) error {
		return nil
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
	startPRWorkspaceSession = func(
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
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	var attached bool
	attachPRWorkspaceSession = func(
		_ context.Context,
		got pullrequest.Workspace,
		gotConfig *models.Config,
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
	withPRCommandDeps(t, cfg, &fakePRService{})
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
	attachPRWorkspaceSession = func(
		_ context.Context,
		got pullrequest.Workspace,
		gotConfig *models.Config,
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
	inspectPRProjectClone = func(
		context.Context,
		pullrequest.Provenance,
	) (pullrequest.Project, []pullrequest.Workspace, error) {
		live := recorded
		live.Branch = "unrelated"
		return project, []pullrequest.Workspace{live}, nil
	}
	attached := false
	attachPRWorkspaceSession = func(
		context.Context,
		pullrequest.Workspace,
		*models.Config,
	) error {
		attached = true
		return nil
	}
	cmd, _, _ := prTestCommand()

	err := runPRAttach(cmd, []string{recorded.Path})

	assertPRCode(t, err, pullrequest.CodeWorkspaceCreation)
	assert.False(t, attached)
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
	recordedInfo, ok := urlutil.CanonicalRepositoryInfo(
		"github.com/current/widget",
	)
	require.True(t, ok)
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
			SessionName: tmux.WorkspaceSessionName(
				recordedInfo,
				branch,
				path,
			),
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
				Path:   livePath,
				Branch: "live",
			},
		},
	)

	require.Len(t, workspaces, 1)
	assert.Equal(t, "live", workspaces[0].Branch)
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
	startPRWorkspaceSession = func(
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
	startPRWorkspaceSession = func(
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
