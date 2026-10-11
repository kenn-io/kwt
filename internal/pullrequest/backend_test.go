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
	shared "go.kenn.io/kwt/worktree"
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

func newRealBackendImport(t *testing.T) (string, *GitBackend, PullRequest) {
	t.Helper()
	repo, backend := newBackendRepo(t)
	runGit(t, repo, "remote", "set-url", "origin", repo)
	runGit(t, repo, "remote", "set-url", "--push", "origin", "https://github.com/acme/widget.git")
	runGit(t, repo, "branch", "feature/widgets")
	pr := testPR(17, false)
	pr.HeadSHA = runGit(t, repo, "rev-parse", "HEAD")
	return repo, backend, pr
}

func TestGitBackendImportsVerifiedHeadAndIdentity(t *testing.T) {
	repo, backend, pr := newRealBackendImport(t)
	workspace, err := backend.ImportPullRequest(t.Context(), pr, "pr-17-feature-widgets")
	require.NoError(t, err)
	assert.Contains(t, filepath.ToSlash(workspace.Path), "github.com/acme/widget/pr-17-feature-widgets")
	assert.Equal(t, "pr-17-feature-widgets", workspace.Branch)
	assert.Equal(t, pr.HeadSHA, runGit(t, workspace.Path, "rev-parse", "HEAD"))
	assert.NoError(t, shared.ValidateWorktreeGeneration(workspace.Generation))
	assert.NotEmpty(t, workspace.ID)
	assert.NotEmpty(t, workspace.SessionName)
	require.NoError(t, backend.Rollback(t.Context(), workspace))
	assert.NoDirExists(t, workspace.Path)
	assert.NotContains(t, runGit(t, repo, "for-each-ref", "--format=%(refname)"), "refs/heads/pr-17-feature-widgets")
}

func TestGitBackendImportedWorktreePreservesMergeConflictContents(t *testing.T) {
	canonicalURL := "https://github.com/acme/widget.git"
	baseContent := "base\n"
	featureContent := "feature change\n"
	mainContent := "main change\n"

	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	projectRepo, backend := newBackendRepo(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(projectRepo, ".gitattributes"),
		[]byte("conflict.txt merge=untrusted\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(projectRepo, "conflict.txt"),
		[]byte(baseContent),
		0o644,
	))
	runGit(t, projectRepo, "add", ".gitattributes", "conflict.txt")
	runGit(t, projectRepo, "commit", "-m", "add merge-driver fixture")
	runGit(t, projectRepo, "config", "merge.untrusted.driver",
		"f() { return 1; }; f")

	runGit(t, projectRepo, "checkout", "-q", "-b", "feature/widgets")
	require.NoError(t, os.WriteFile(
		filepath.Join(projectRepo, "conflict.txt"),
		[]byte(featureContent),
		0o644,
	))
	runGit(t, projectRepo, "commit", "-am", "feature change")
	headSHA := runGit(t, projectRepo, "rev-parse", "HEAD")

	runGit(t, projectRepo, "checkout", "-q", "main")
	require.NoError(t, os.WriteFile(
		filepath.Join(projectRepo, "conflict.txt"),
		[]byte(mainContent),
		0o644,
	))
	runGit(t, projectRepo, "commit", "-am", "main change")
	mainSHA := runGit(t, projectRepo, "rev-parse", "main")

	runGit(t, projectRepo, "remote", "set-url", "origin", projectRepo)
	runGit(t, projectRepo, "remote", "set-url", "--push", "origin", canonicalURL)

	pr := testPR(17, false)
	pr.HeadSHA = headSHA
	pr.Source.Repository.CloneURL = canonicalURL

	workspace, err := backend.ImportPullRequest(
		t.Context(), pr, "pr-17-feature-widgets",
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = backend.Rollback(t.Context(), workspace)
	})

	merge := exec.Command("git", "merge", "--no-edit", mainSHA)
	merge.Dir = workspace.Path
	merge.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	mergeOutput, err := merge.CombinedOutput()
	require.Error(t, err, "the merge must report the overlapping change: %s", mergeOutput)

	assert.Equal(t, "UU conflict.txt",
		runGit(t, workspace.Path, "status", "--short"))
	contents, err := os.ReadFile(filepath.Join(workspace.Path, "conflict.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(contents), "<<<<<<< current")
	assert.Contains(t, string(contents), "||||||| base")
	assert.Contains(t, string(contents), "=======")
	assert.Contains(t, string(contents), ">>>>>>> other")
	assert.Contains(t, string(contents), featureContent)
	assert.Contains(t, string(contents), mainContent)
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
	_, runner, err := backend.openWorktrees(t.Context())
	require.NoError(t, err)
	credentialOutput, _, credentialErr := runner.Run(t.Context(), repo, strings.NewReader("url=https://example.com/acme/widget.git\n\n"), "credential", "fill")

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
			repo, backend, pr := newRealBackendImport(t)
			runGit(t, repo, "config", tc.key, tc.value)
			path, err := backend.manager.PreparePathForRepository("", "pr-17-feature-widgets", backend.project.Identity)
			require.NoError(t, err)
			workspace, err := backend.ImportPullRequest(t.Context(), pr, "pr-17-feature-widgets")
			assertErrorCode(t, err, CodeWorkspaceCreation)
			assert.Empty(t, workspace.Path)
			assert.NoDirExists(t, path)
			assert.NotContains(t, runGit(t, repo, "for-each-ref", "--format=%(refname)"), "refs/heads/pr-17-feature-widgets")
		})
	}
}

func TestGitBackendForkImportWithoutTrackingRejectsExplicitOriginPush(t *testing.T) {
	repo, backend, _ := newRealBackendImport(t)
	fork := t.TempDir()
	runGit(t, fork, "init", "--bare", "-b", "main")
	request := testPR(17, true)
	request.Source.Repository.CloneURL = fork
	request.HeadSHA = runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "update-ref", "refs/pull/17/head", request.HeadSHA)
	runGit(t, repo, "config", "push.default", "current")
	runGit(t, repo, "config", "remote.origin.push", "HEAD:refs/heads/main")
	workspace, err := backend.ImportPullRequest(t.Context(), request, "pr-17-feature-widgets")
	assertErrorCode(t, err, CodeWorkspaceCreation)
	require.ErrorContains(t, err, "failed to validate pull-request push routing")
	assert.Empty(t, workspace.Path)
	assert.NotContains(t, runGit(t, repo, "for-each-ref", "--format=%(refname)"), "refs/heads/pr-17-feature-widgets")
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
