package pullrequest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	managedworktree "go.kenn.io/kit/git/managed"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), output)
	return strings.TrimSpace(string(output))
}

func newBackendRepo(t *testing.T) (string, *GitBackend) {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644))
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	runGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	g := gitadapter.New(repo)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir: filepath.Join(t.TempDir(), "worktrees"), AutoMkdir: true,
		},
		Projects: []models.Project{{
			Repository: testProject().Identity, Name: testProject().Name, Path: repo,
		}},
	}
	return repo, NewGitBackend(
		g, worktree.New(g, cfg), testProject(), nil, cfg.Fleet.TokenEnv,
	)
}

type recordingCreationGuard struct {
	path   string
	active bool
}

func (g *recordingCreationGuard) AcquireCreation(
	path string,
) (func() error, bool, error) {
	g.path = path
	g.active = true
	return func() error {
		g.active = false
		return nil
	}, true, nil
}

func TestGitBackendCoordinatesCreationForResolvedWorkspacePath(t *testing.T) {
	_, backend := newBackendRepo(t)
	guard := &recordingCreationGuard{}
	backend.openCreationGuard = func() (WorktreeCreationGuard, error) {
		return guard, nil
	}
	activeDuringOperation := false

	release, err := backend.AcquireWorkspaceCreation(
		context.Background(),
		"pr-17-feature-widgets",
	)

	require.NoError(t, err)
	activeDuringOperation = guard.active
	assert.True(t, activeDuringOperation)
	assert.Contains(t, filepath.ToSlash(guard.path), "github.com/acme/widget/pr-17-feature-widgets")
	require.NoError(t, release())
	assert.False(t, guard.active)
}

func configureTestPushTracking(
	t *testing.T,
	repo, branch, remote, remoteURL string,
) {
	t.Helper()
	require.NoError(t, os.MkdirAll(repo, 0o755))
	runGit(t, repo, "init", "-b", branch)
	runGit(t, repo, "remote", "add", remote, remoteURL)
	runGit(t, repo, "config", "branch."+branch+".remote", remote)
	runGit(t, repo, "config",
		"branch."+branch+".merge", "refs/heads/feature/widgets")
	runGit(t, repo, "config", "branch."+branch+".pushRemote", remote)
	runGit(t, repo, "config", "push.default", "upstream")
}

func newTestPushRunner(t *testing.T) (gitcmd.Runner, string) {
	t.Helper()
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(configDir, "global.gitconfig"), nil, 0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(configDir, "system.gitconfig"), nil, 0o600,
	))
	runner := gitcmd.New()
	runner.StripEnv = false
	runner.Env = append(
		runner.Env,
		"GIT_CONFIG_GLOBAL="+filepath.Join(configDir, "global.gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(configDir, "system.gitconfig"),
	)
	return runner, filepath.Join(configDir, "global.gitconfig")
}

func TestGitBackendDelegatesPullRequestLifecycleToKit(t *testing.T) {
	repo, backend := newBackendRepo(t)
	runGit(t, repo, "remote", "set-url", "origin",
		"https://github.com/octocat/widget.git")
	var got managedworktree.MergeRequestWorktreeOptions
	original := createMergeRequestWorktree
	createMergeRequestWorktree = func(
		_ context.Context, opts managedworktree.MergeRequestWorktreeOptions,
	) (managedworktree.CreateWorktreeResult, error) {
		got = opts
		configureTestPushTracking(
			t, opts.Path, opts.Branch, "origin",
			"https://github.com/acme/widget.git",
		)
		return managedworktree.CreateWorktreeResult{
			Path: opts.Path, Branch: opts.Branch, BranchCreated: true,
		}, nil
	}
	t.Cleanup(func() { createMergeRequestWorktree = original })
	pr := testPR(17, false)

	workspace, err := backend.ImportPullRequest(
		context.Background(), pr, "pr-17-feature-widgets",
	)

	require.NoError(t, err)
	resolvedRepo, resolveErr := filepath.EvalSymlinks(repo)
	require.NoError(t, resolveErr)
	assert.Equal(t, resolvedRepo, got.ProjectRoot)
	assert.Equal(t, "pr-17-feature-widgets", got.Branch)
	assert.Equal(t, 17, got.Number)
	assert.Equal(t, pr.Source.Name, got.HeadBranch)
	assert.Equal(t, pr.Source.Repository.CloneURL, got.HeadRepoCloneURL)
	assert.Equal(t, pr.HeadSHA, got.ExpectedHeadSHA)
	assert.Equal(t, "github", got.Platform)
	assert.Equal(t, testProject().Identity, got.ProjectRepoIdentity)
	assert.Equal(t, "KWT", got.HookEnvironmentPrefix)
	assert.Contains(t, filepath.ToSlash(got.Path), "github.com/acme/widget/pr-17-feature-widgets")
	assert.Equal(t, got.Path, workspace.Path)
	assert.Equal(t, "pr-17-feature-widgets", workspace.Branch)
	assert.NoError(t, gitadapter.ValidateWorktreeGeneration(workspace.Generation))
	assert.NotEmpty(t, workspace.ID)
	assert.NotEmpty(t, workspace.SessionName)
}

func TestGitBackendMakesCredentialHelpersAvailableToLifecycle(t *testing.T) {
	configDir := t.TempDir()
	helper := filepath.Join(configDir, "credential-helper")
	require.NoError(t, os.WriteFile(helper, []byte(`#!/bin/sh
while IFS= read -r line && [ -n "$line" ]; do :; done
if [ "$1" = get ]; then
	printf 'username=helper-user\npassword=helper-token\n'
fi
`), 0o755))
	globalConfig := filepath.Join(configDir, ".gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte(
		"[credential \"https://example.com\"]\n\thelper = !"+helper+"\n",
	), 0o600))
	t.Setenv("HOME", configDir)

	repo, backend := newBackendRepo(t)
	var credentialOutput []byte
	var credentialErr error
	original := createMergeRequestWorktree
	createMergeRequestWorktree = func(
		ctx context.Context, opts managedworktree.MergeRequestWorktreeOptions,
	) (managedworktree.CreateWorktreeResult, error) {
		assert.False(t, opts.Runner.NullGlobalConfig)
		assert.False(t, opts.Runner.NoSystemConfig)
		assert.True(t, opts.Runner.StripEnv)
		assert.False(t, opts.Runner.TerminalPrompt)
		credentialOutput, _, credentialErr = opts.Runner.Run(
			ctx,
			repo,
			strings.NewReader("url=https://example.com/acme/widget.git\n\n"),
			"credential", "fill",
		)
		return managedworktree.CreateWorktreeResult{}, errors.New("stop after credential lookup")
	}
	t.Cleanup(func() { createMergeRequestWorktree = original })

	_, _ = backend.ImportPullRequest(
		t.Context(), testPR(17, false), "pr-17-feature-widgets",
	)

	require.NoError(t, credentialErr)
	assert.Contains(t, string(credentialOutput), "username=helper-user")
	assert.Contains(t, string(credentialOutput), "password=helper-token")
}

func TestGitBackendSameRepositoryImportRejectsBroadPush(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{
			name:  "remote push refspec",
			key:   "remote.origin.push",
			value: "HEAD:refs/heads/main",
		},
		{
			name:  "follow tags",
			key:   "push.followTags",
			value: "true",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, backend := newBackendRepo(t)
			original := createMergeRequestWorktree
			createMergeRequestWorktree = func(
				_ context.Context,
				opts managedworktree.MergeRequestWorktreeOptions,
			) (managedworktree.CreateWorktreeResult, error) {
				configureTestPushTracking(
					t, opts.Path, opts.Branch, "origin",
					"https://github.com/acme/widget.git",
				)
				runGit(t, opts.Path, "config", tc.key, tc.value)
				return managedworktree.CreateWorktreeResult{
					Path: opts.Path, Branch: opts.Branch, BranchCreated: true,
				}, nil
			}
			t.Cleanup(func() { createMergeRequestWorktree = original })

			workspace, err := backend.ImportPullRequest(
				t.Context(), testPR(17, false), "pr-17-feature-widgets",
			)

			assertErrorCode(t, err, CodeWorkspaceCreation)
			assert.NotEmpty(t, workspace.Path)
		})
	}
}

func TestGitBackendForkImportWithoutTrackingRejectsExplicitOriginPush(t *testing.T) {
	_, backend := newBackendRepo(t)
	original := createMergeRequestWorktree
	createMergeRequestWorktree = func(
		_ context.Context, opts managedworktree.MergeRequestWorktreeOptions,
	) (managedworktree.CreateWorktreeResult, error) {
		require.NoError(t, os.MkdirAll(opts.Path, 0o755))
		runGit(t, opts.Path, "init", "-b", opts.Branch)
		runGit(t, opts.Path, "config", "push.default", "current")
		runGit(t, opts.Path, "config", "remote.origin.push",
			"HEAD:refs/heads/main")
		runGit(t, opts.Path, "config", "extensions.worktreeConfig", "true")
		return managedworktree.CreateWorktreeResult{
			Path: opts.Path, Branch: opts.Branch, BranchCreated: true,
		}, nil
	}
	t.Cleanup(func() { createMergeRequestWorktree = original })

	workspace, err := backend.ImportPullRequest(
		context.Background(), testPR(17, true), "pr-17-feature-widgets",
	)

	assertErrorCode(t, err, CodeWorkspaceCreation)
	assert.NotEmpty(t, workspace.Path)
}

func TestEnsurePullRequestPushSafetyValidatesEffectiveDestination(t *testing.T) {
	repo := t.TempDir()
	configureTestPushTracking(
		t, repo, "pr-17-feature-widgets", "fork",
		"https://github.com/octocat/widget.git",
	)
	runner, globalConfig := newTestPushRunner(t)

	err := ensurePullRequestPushSafety(
		t.Context(), runner, repo, "pr-17-feature-widgets",
		"github.com/octocat/widget", "feature/widgets",
	)
	require.NoError(t, err)

	runGit(t, repo, "config", "remote.fork.pushurl",
		"https://github.com/acme/widget.git")
	err = ensurePullRequestPushSafety(
		t.Context(), runner, repo, "pr-17-feature-widgets",
		"github.com/octocat/widget", "feature/widgets",
	)
	require.Error(t, err)

	runGit(t, repo, "config", "--unset", "remote.fork.pushurl")
	require.NoError(t, os.WriteFile(
		globalConfig,
		[]byte("[push]\n\tfollowTags = true\n"),
		0o600,
	))
	err = ensurePullRequestPushSafety(
		t.Context(), runner, repo, "pr-17-feature-widgets",
		"github.com/octocat/widget", "feature/widgets",
	)
	require.Error(t, err)
}

func TestEnsurePullRequestPushSafetyValidatesTrustedProjectAuthority(t *testing.T) {
	for _, tc := range []struct {
		name       string
		projectURL string
		sourceURL  string
		wantErr    bool
	}{
		{
			name:       "SSH alias",
			projectURL: "git@github-work:acme/widget.git",
			sourceURL:  "git@github-work:octocat/widget.git",
		},
		{
			name:       "explicit SSH port",
			projectURL: "ssh://git@github.com:22/acme/widget.git",
			sourceURL:  "ssh://git@github.com:22/octocat/widget.git",
		},
		{
			name:       "unrelated SSH alias",
			projectURL: "git@github-work:acme/widget.git",
			sourceURL:  "git@github-other:octocat/widget.git",
			wantErr:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			configureTestPushTracking(
				t, repo, "pr-17-feature-widgets", "fork", tc.sourceURL,
			)
			runGit(t, repo, "remote", "add", "origin", tc.projectURL)

			runner, _ := newTestPushRunner(t)
			err := ensurePullRequestPushSafety(
				t.Context(), runner, repo,
				"pr-17-feature-widgets",
				"github.com/octocat/widget", "feature/widgets",
			)

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGitBackendMapsSharedLifecycleErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code ErrorCode
	}{
		{name: "authentication", err: &managedworktree.ChangeRequestError{
			Kind: managedworktree.ChangeRequestAuthentication, Message: "authentication failed",
		}, code: CodeAuthentication},
		{name: "network", err: &managedworktree.ChangeRequestError{
			Kind: managedworktree.ChangeRequestNetwork, Message: "network failed",
		}, code: CodeNetwork},
		{name: "head", err: &managedworktree.ChangeRequestError{
			Kind: managedworktree.ChangeRequestInaccessibleHead, Message: "head missing",
		}, code: CodeInaccessibleHead},
		{name: "changed", err: &managedworktree.ChangeRequestError{
			Kind: managedworktree.ChangeRequestHeadChanged, Message: "head changed",
		}, code: CodeConflict},
		{name: "git", err: &managedworktree.ChangeRequestError{
			Kind: managedworktree.ChangeRequestUnsupportedGit, Message: "git unsupported",
		}, code: CodeUnsupportedGitVersion},
		{name: "branch", err: managedworktree.ErrBranchInUse, code: CodeNamingConflict},
		{name: "path", err: managedworktree.ErrWorktreeDestinationExists, code: CodeNamingConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapped := mapSharedChangeRequestError(tc.err)
			var typed *Error
			require.ErrorAs(t, mapped, &typed)
			assert.Equal(t, tc.code, typed.Code)
		})
	}
}

func TestSafeGitEnvironmentRemovesKWTSecrets(t *testing.T) {
	environment := []string{
		"PATH=/bin", "KWT_GITHUB_TOKEN=secret", "KWT_FLEET_TOKEN=fleet",
		"KWT_HOME=/private/kwt", "AWS_SECRET_ACCESS_KEY=custom-fleet-token",
		"VISIBLE=yes",
	}

	got := SafeGitEnvironment(environment, "aws_secret_access_key")

	assert.Equal(t, []string{"PATH=/bin", "VISIBLE=yes"}, got)
}

func TestGitBackendListWorkspacesOmitsMissingRegistrations(t *testing.T) {
	repo, backend := newBackendRepo(t)
	path := filepath.Join(t.TempDir(), "missing")
	runGit(t, repo, "worktree", "add", "-b", "missing-worktree", path)
	require.NoError(t, os.RemoveAll(path))

	workspaces, err := backend.ListWorkspaces(context.Background())

	require.NoError(t, err)
	for _, candidate := range workspaces {
		assert.NotEqual(t, path, candidate.Path)
	}
}

func TestMapSharedChangeRequestErrorPreservesUnknownErrors(t *testing.T) {
	want := errors.New("application failure")
	assert.ErrorIs(t, mapSharedChangeRequestError(want), want)
}
