package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func withProjectsConfig(t *testing.T, projects []models.Project) {
	t.Helper()
	origLoad := loadProjectsConfig
	t.Cleanup(func() { loadProjectsConfig = origLoad })
	loadProjectsConfig = func() (*models.Config, error) {
		return &models.Config{Projects: projects}, nil
	}
	origQuery := queryProjectsInventory
	t.Cleanup(func() { queryProjectsInventory = origQuery })
	queryProjectsInventory = func(
		context.Context,
		kwt.Request,
		bool,
		io.Writer,
	) (kwt.Result, error) {
		published := make([]kwt.Project, 0, len(projects))
		for _, project := range projects {
			identity, err := lifecycle.ResolveProjectRegistrationIdentity(
				context.Background(),
				config.ProjectRegistration{Persisted: project, Effective: project},
			)
			if err != nil {
				continue
			}
			published = append(published, kwt.Project{
				Repository:              identity,
				Name:                    project.Name,
				Path:                    project.Path,
				LastTouched:             project.LastTouched,
				RegistrationFingerprint: "v1:0000000000000000000000000000000000000000000000000000000000000000",
			})
		}
		return kwt.Result{Snapshot: kwt.Snapshot{
			Projects: published,
		}}, nil
	}
}

func runProjectsForTest(t *testing.T, jsonOut bool) string {
	t.Helper()
	prev := projectsJSON
	projectsJSON = jsonOut
	t.Cleanup(func() { projectsJSON = prev })

	buf := &bytes.Buffer{}
	projectsCmd.SetOut(buf)
	if err := runProjects(projectsCmd, nil); err != nil {
		t.Fatalf("runProjects error = %v", err)
	}
	return buf.String()
}

// projectsCmd overrides the root PersistentPreRunE (which merges the caller's
// cwd .kwt.toml): projects is a global registry surface, and a repository
// config in the caller's cwd must not be able to prompt for trust or fail the
// documented --json output.
func TestProjectsCommandSkipsCwdConfigMerge(t *testing.T) {
	require.NotNil(t, projectsCmd.PersistentPreRunE,
		"projects must define its own PersistentPreRunE to bypass root's cwd merge")
	require.NoError(t, projectsCmd.PersistentPreRunE(projectsCmd, nil),
		"projects's PersistentPreRunE must be a no-op that never errors")
}

func TestRunProjectsJSONEmitsRegistry(t *testing.T) {
	repoPath := newTUITestRepo(t)
	withProjectsConfig(t, []models.Project{
		{
			Repository:  "github.com/wesm/kwt",
			Name:        "kwt",
			Path:        repoPath,
			LastTouched: "2026-07-16T00:00:00Z",
		},
	})

	out := runProjectsForTest(t, true)

	var got []kwt.Project
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal JSON output: %v (out=%q)", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 project, got %d", len(got))
	}
	if got[0].Repository != "github.com/wesm/kwt" || got[0].Name != "kwt" ||
		got[0].Path != repoPath || got[0].LastTouched != "2026-07-16T00:00:00Z" {
		t.Errorf("unexpected project: %+v", got[0])
	}
	assert.Equal(t, "v1:0000000000000000000000000000000000000000000000000000000000000000", got[0].RegistrationFingerprint)
}

func TestRunProjectsRendersTable(t *testing.T) {
	repoPath := newTUITestRepo(t)
	withProjectsConfig(t, []models.Project{{
		Repository:  "github.com/wesm/kwt",
		Name:        "kwt",
		Path:        repoPath,
		LastTouched: "2026-07-16T00:00:00Z",
	}})

	out := runProjectsForTest(t, false)

	assert.Contains(t, out, "NAME")
	assert.Contains(t, out, "REPOSITORY")
	assert.Contains(t, out, "github.com/wesm/kwt")
	assert.Contains(t, out, repoPath)
}

func TestRunProjectsReturnsConfigLoadError(t *testing.T) {
	origQuery := queryProjectsInventory
	t.Cleanup(func() { queryProjectsInventory = origQuery })
	queryProjectsInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{}, errors.New("config unavailable")
	}

	err := runProjects(projectsCmd, nil)

	require.EqualError(t, err, "config unavailable")
}

func TestRunProjectsJSONWritesStableInventoryFailure(t *testing.T) {
	originalQuery := queryProjectsInventory
	t.Cleanup(func() { queryProjectsInventory = originalQuery })
	queryProjectsInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{}, service.NewError(
			service.InventoryTimeout, "inventory refresh timed out", true, nil, context.DeadlineExceeded,
		)
	}
	previousJSON := projectsJSON
	projectsJSON = true
	t.Cleanup(func() { projectsJSON = previousJSON })
	var stdout, stderr bytes.Buffer
	projectsCmd.SetOut(&stdout)
	projectsCmd.SetErr(&stderr)

	err := runProjects(projectsCmd, nil)

	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 1, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.InventoryTimeout, envelope.Error.Code)
	assert.True(t, envelope.Error.Retryable)
	assert.Contains(t, stderr.String(), "inventory_timeout")
}

// TestRunProjectsJSONCanonicalizesLegacyAbsolutePathIdentity pins the fix for
// registry entries created before the canonical "local/..." form existed:
// they retain an absolute-path Repository, which projects --json must not
// emit verbatim (list --json emits the canonical local/... form for the same
// no-remote repository, so a raw path would break joins between the two
// surfaces). Canonicalization happens at emission time only; the registry
// itself is not mutated.
func TestRunProjectsJSONCanonicalizesLegacyAbsolutePathIdentity(t *testing.T) {
	repoPath := newTUITestRepo(t)
	canonical, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)

	legacy := []models.Project{{
		Repository:  repoPath,
		Name:        "service-api",
		Path:        repoPath,
		LastTouched: "2026-07-16T00:00:00Z",
	}}
	withProjectsConfig(t, legacy)

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 1)
	assert.Equal(t, canonical.FullPath, got[0].Repository,
		"emitted repository must match the canonical local/... resolver output")
	assert.Equal(t, repoPath, legacy[0].Repository,
		"emission-time canonicalization must not mutate the registry entry")
}

// TestRunProjectsResolvesLegacyPathEntryThroughCurrentOrigin pins that a
// legacy path-registered entry whose repository later gained a remote emits
// the origin-derived slug, not the local/... path fallback: kwt list --json
// resolves the same repository through its origin, so emitting the path
// fallback here would break joins between the two surfaces.
func TestRunProjectsResolvesLegacyPathEntryThroughCurrentOrigin(t *testing.T) {
	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "git@github.com:org/legacy.git")

	listSide, err := worktree.RepositoryInfoFromGit(git.New(repoPath))
	require.NoError(t, err)
	require.Equal(t, "github.com/org/legacy", listSide.FullPath)

	withProjectsConfig(t, []models.Project{{
		Repository: repoPath,
		Name:       "legacy",
		Path:       repoPath,
	}})

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 1)
	assert.Equal(t, listSide.FullPath, got[0].Repository,
		"legacy path entry must join with the list --json identity for the same repository")
}

// TestRunProjectsRelativeDotlessRemoteJoinsListOnLocalIdentity pins the
// provenance gate on the projects re-resolution path: a legacy noncanonical
// entry whose repository's origin is a relative dotless filesystem remote
// ("cache/team/repo.git" — git accepts it with no leading "./") must emit the
// canonical local/... fallback, not a shareable-looking "cache/team/repo"
// slug, and must join with the identity kwt list --json derives for the same
// repository through the shared resolver.
func TestRunProjectsRelativeDotlessRemoteJoinsListOnLocalIdentity(t *testing.T) {
	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "cache/team/repo.git")

	local, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)
	listSide, err := worktree.RepositoryInfoFromGit(git.New(repoPath))
	require.NoError(t, err)
	require.Equal(t, local.FullPath, listSide.FullPath,
		"the shared resolver must reject a relative dotless remote and fall back to local identity")

	withProjectsConfig(t, []models.Project{{
		Repository: repoPath,
		Name:       "repo",
		Path:       repoPath,
	}})

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 1)
	assert.Equal(t, listSide.FullPath, got[0].Repository,
		"projects --json must join with list --json on the local fallback identity")
	assert.NotEqual(t, "cache/team/repo", got[0].Repository)
}

// TestRunProjectsConfiguredIdentityIsAuthoritative pins the configured-bar
// policy: a stored identity that passes the canonical bar is emitted as-is,
// even when the repository's current origin would fail the remote bar. kwt
// does not second-guess deliberate registry values against remote provenance.
func TestRunProjectsConfiguredIdentityIsAuthoritative(t *testing.T) {
	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "cache/team/repo.git")

	withProjectsConfig(t, []models.Project{{
		Repository: "cache/team/repo",
		Name:       "repo",
		Path:       repoPath,
	}})

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "cache/team/repo", got[0].Repository)
}

// TestRunProjectsJSONLeavesCanonicalSlugUntouched confirms an
// already-canonical host/owner/name slug (the common case) is emitted
// exactly as registered.
func TestRunProjectsJSONLeavesCanonicalSlugUntouched(t *testing.T) {
	repoPath := newTUITestRepo(t)
	withProjectsConfig(t, []models.Project{{
		Repository: "github.com/wesm/kwt",
		Name:       "kwt",
		Path:       repoPath,
	}})

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "github.com/wesm/kwt", got[0].Repository)
}

// TestRunProjectsNormalizesRemoteURLIdentities pins that every publishable
// repository value goes through the canonical resolver: remote URLs are
// emitted as the same host/owner/name slug kwt list --json reports, so the
// two surfaces stay joinable regardless of the URL form in the registry.
func TestRunProjectsNormalizesRemoteURLIdentities(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		want       string
	}{
		{name: "https URL", repository: "https://github.com/wesm/kwt.git", want: "github.com/wesm/kwt"},
		{name: "scp-style git@ URL", repository: "git@github.com:wesm/kwt.git", want: "github.com/wesm/kwt"},
		{name: "git scheme URL", repository: "git://github.com/wesm/kwt.git", want: "github.com/wesm/kwt"},
		{
			name:       "credential-bearing https URL",
			repository: "https://wesm:ghp_secret123@github.com/wesm/kwt.git",
			want:       "github.com/wesm/kwt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoPath := newTUITestRepo(t)
			withProjectsConfig(t, []models.Project{{
				Repository: tt.repository,
				Name:       "kwt",
				Path:       repoPath,
			}})

			out := runProjectsForTest(t, true)

			var got []models.Project
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, got[0].Repository)
		})
	}
}

// TestRunProjectsNeverEmitsRegistryCredentials pins that a credential-bearing
// registry value never reaches either output surface: URL userinfo is
// stripped by canonical normalization, and a value the resolver rejects
// (scp-style user:token) is replaced by the path-derived local identity
// rather than emitted raw.
func TestRunProjectsNeverEmitsRegistryCredentials(t *testing.T) {
	const token = "ghp_secret123"
	repoPath := newTUITestRepo(t)
	local, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)

	tests := []struct {
		name       string
		repository string
		want       string
	}{
		{
			name:       "https userinfo stripped",
			repository: "https://wesm:" + token + "@github.com/wesm/kwt.git",
			want:       "github.com/wesm/kwt",
		},
		{
			name:       "scp-style user:token falls back to local path identity",
			repository: "wesm:" + token + "@github.com:wesm/kwt.git",
			want:       local.FullPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := models.Project{Repository: tt.repository, Name: "kwt", Path: repoPath}
			withProjectsConfig(t, []models.Project{project})

			for _, jsonOut := range []bool{true, false} {
				out := runProjectsForTest(t, jsonOut)
				assert.NotContains(t, out, token,
					"credential must not appear in output (json=%v)", jsonOut)
				assert.Contains(t, out, tt.want, "expected canonical identity (json=%v)", jsonOut)
			}
		})
	}
}

func TestRunProjectsRetainsRegistrationsWithUnavailableCheckouts(t *testing.T) {
	livePath := newTUITestRepo(t)
	missingPath := filepath.Join(t.TempDir(), "missing")
	nestedPath := filepath.Join(livePath, "nested")
	require.NoError(t, os.Mkdir(nestedPath, 0o755))
	withProjectsConfig(t, []models.Project{
		{Repository: "github.com/example/live", Name: "live", Path: livePath},
		{Repository: "github.com/example/missing", Name: "missing", Path: missingPath},
		{Repository: "github.com/example/nested", Name: "nested", Path: nestedPath},
		{Repository: "local/pathless", Name: "pathless"},
	})

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 3)
	assert.Equal(t, []string{"live", "missing", "nested"}, []string{
		got[0].Name, got[1].Name, got[2].Name,
	})
	assert.Equal(t, missingPath, got[1].Path)
}

func TestRunProjectsJSONEmptyIsArray(t *testing.T) {
	withProjectsConfig(t, nil)

	out := runProjectsForTest(t, true)

	var got []models.Project
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal JSON output: %v (out=%q)", err, out)
	}
	require.NotNil(t, got)
	if len(got) != 0 {
		t.Errorf("expected empty array, got %d entries", len(got))
	}
}

func TestRunProjectsJSONPathlessRegistrationIsFilteredArray(t *testing.T) {
	withProjectsConfig(t, []models.Project{
		{Repository: "local/pathless", Name: "pathless"},
	})

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotNil(t, got)
	assert.Empty(t, got)
}

func TestRunProjectsAddJSONRegistersCanonicalProject(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	repoPath := newTUITestRepo(t)
	canonicalRepoPath, err := filepath.EvalSymlinks(repoPath)
	require.NoError(t, err)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "git@github.com:acme/widget.git")
	linkedPath := filepath.Join(t.TempDir(), "linked")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "linked", linkedPath)

	originalRegister := registerProject
	t.Cleanup(func() {
		registerProject = originalRegister
	})
	var registered models.Project
	registerProject = func(_ context.Context, project models.Project) (kwt.Project, error) {
		registered = project
		return kwt.Project{
			Repository:              project.Repository,
			Name:                    project.Name,
			Path:                    project.Path,
			LastTouched:             project.LastTouched,
			RegistrationFingerprint: "v1:0000000000000000000000000000000000000000000000000000000000000000",
		}, nil
	}
	projectsAddJSON = true
	t.Cleanup(func() { projectsAddJSON = false })
	stdout := &bytes.Buffer{}
	projectsAddCmd.SetOut(stdout)

	require.NoError(t, runProjectsAdd(projectsAddCmd, []string{linkedPath}))

	var response struct {
		Status  string `json:"status"`
		Project struct {
			models.Project
			RegistrationFingerprint string `json:"registration_fingerprint"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	assert.Equal(t, "registered", response.Status)
	assert.Equal(t, "github.com/acme/widget", response.Project.Repository)
	assert.Equal(t, canonicalRepoPath, response.Project.Path)
	assert.NotEmpty(t, response.Project.LastTouched)
	assert.Regexp(t, `^v1:[0-9a-f]{64}$`, response.Project.RegistrationFingerprint)
	assert.Equal(t, response.Project.Project, registered)
}

func TestResolveProjectForRegistrationAcceptsBareContainer(t *testing.T) {
	containerPath, mainPath := newTUIBareContainerRepo(t)
	canonicalMainPath, err := filepath.EvalSymlinks(mainPath)
	require.NoError(t, err)

	project, err := resolveProjectForRegistration(containerPath)

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", project.Repository)
	assert.Equal(t, "widget", project.Name)
	assert.Equal(t, canonicalMainPath, project.Path)
}

func TestResolveProjectForRegistrationPrefersNestedBareContainer(t *testing.T) {
	outerPath := newTUITestRepo(t)
	containerPath := filepath.Join(outerPath, "nested", "widget")
	mainPath := newTUIBareContainerRepoAt(t, containerPath)
	canonicalMainPath, err := filepath.EvalSymlinks(mainPath)
	require.NoError(t, err)

	project, err := resolveProjectForRegistration(containerPath)

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", project.Repository)
	assert.Equal(t, "widget", project.Name)
	assert.Equal(t, canonicalMainPath, project.Path)
}

func TestResolveProjectForRegistrationIgnoresRegularMainRepository(t *testing.T) {
	projectPath := newTUITestRepo(t)
	runTUITestGit(
		t,
		projectPath,
		"remote",
		"add",
		"origin",
		"git@github.com:acme/project.git",
	)
	nestedMainPath := filepath.Join(projectPath, "main")
	runTUITestGit(t, "", "init", "-b", "main", nestedMainPath)
	runTUITestGit(t, nestedMainPath, "config", "user.name", "Test User")
	runTUITestGit(t, nestedMainPath, "config", "user.email", "test@example.com")
	runTUITestGit(t, nestedMainPath, "commit", "--allow-empty", "-m", "Initial commit")
	runTUITestGit(
		t,
		nestedMainPath,
		"remote",
		"add",
		"origin",
		"git@github.com:acme/nested.git",
	)
	t.Chdir(projectPath)
	canonicalProjectPath, err := filepath.EvalSymlinks(projectPath)
	require.NoError(t, err)

	project, err := resolveProjectForRegistration(projectPath)

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/project", project.Repository)
	assert.Equal(t, "project", project.Name)
	assert.Equal(t, canonicalProjectPath, project.Path)
}

func newTUIBareContainerRepo(t *testing.T) (string, string) {
	t.Helper()

	containerPath := filepath.Join(t.TempDir(), "widget")
	return containerPath, newTUIBareContainerRepoAt(t, containerPath)
}

func newTUIBareContainerRepoAt(t *testing.T, containerPath string) string {
	t.Helper()

	seedPath := newTUITestRepo(t)
	barePath := filepath.Join(containerPath, ".bare")
	mainPath := filepath.Join(containerPath, "main")
	require.NoError(t, os.MkdirAll(containerPath, 0755))
	runTUITestGit(t, "", "clone", "--bare", seedPath, barePath)
	runTUITestGit(
		t,
		barePath,
		"remote",
		"set-url",
		"origin",
		"git@github.com:acme/widget.git",
	)
	runTUITestGit(t, barePath, "worktree", "add", mainPath, "main")
	return mainPath
}

func TestRunProjectsAddJSONWritesStableInvalidRepositoryError(t *testing.T) {
	projectsAddJSON = true
	t.Cleanup(func() { projectsAddJSON = false })
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	projectsAddCmd.SetOut(stdout)
	projectsAddCmd.SetErr(stderr)

	err := runProjectsAdd(
		projectsAddCmd,
		[]string{filepath.Join(t.TempDir(), "missing")},
	)

	var exitErr interface{ ExitCode() int }
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 2, exitErr.ExitCode())
	var response struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	assert.Equal(t, "invalid_repository", response.Error.Code)
	assert.False(t, response.Error.Retryable)
	assert.NotEmpty(t, response.Error.Message)
	assert.Contains(t, stderr.String(), "invalid_repository")
}

func TestRunProjectsRemoveJSONUnregistersByExactIdentity(t *testing.T) {
	originalRemove := removeProjectThroughDaemon
	t.Cleanup(func() { removeProjectThroughDaemon = originalRemove })
	removed := models.Project{
		Repository: "github.com/acme/widget",
		Name:       "widget",
		Path:       "/code/widget",
	}
	var requested kwt.ProjectRemovalRequest
	removeProjectThroughDaemon = func(_ context.Context, request kwt.ProjectRemovalRequest) (kwt.ProjectRemovalResult, error) {
		requested = request
		return kwt.ProjectRemovalResult{Project: removed}, nil
	}
	projectsExpectedRepository = "github.com/acme/widget"
	t.Cleanup(func() { projectsExpectedRepository = "" })
	projectsExpectedRegistration = "v1:1111111111111111111111111111111111111111111111111111111111111111"
	t.Cleanup(func() { projectsExpectedRegistration = "" })
	projectsRemoveJSON = true
	t.Cleanup(func() { projectsRemoveJSON = false })
	stdout := &bytes.Buffer{}
	projectsRemoveCmd.SetOut(stdout)

	require.NoError(t, runProjectsRemove(
		projectsRemoveCmd,
		[]string{"/code/widget "},
	))

	var response projectMutationResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	assert.Equal(t, "/code/widget ", requested.Path)
	assert.Equal(t, "github.com/acme/widget", requested.ExpectedRepository)
	assert.Equal(t, projectsExpectedRegistration, requested.ExpectedRegistration)
	assert.NotEmpty(t, requested.Expansion.HomeDirectory)
	assert.Equal(t, "unregistered", response.Status)
	assert.Equal(t, removed, response.Project)
}

func TestRunProjectsRemoveJSONNeverEmitsRegistryCredentials(t *testing.T) {
	const token = "ghp_secret123"
	originalRemove := removeProjectThroughDaemon
	t.Cleanup(func() { removeProjectThroughDaemon = originalRemove })
	removeProjectThroughDaemon = func(context.Context, kwt.ProjectRemovalRequest) (kwt.ProjectRemovalResult, error) {
		return kwt.ProjectRemovalResult{Project: models.Project{
			Repository: "github.com/acme/widget",
			Name:       "widget",
			Path:       "/code/widget",
		}}, nil
	}
	projectsExpectedRepository = "github.com/acme/widget"
	t.Cleanup(func() { projectsExpectedRepository = "" })
	projectsExpectedRegistration = "v1:1111111111111111111111111111111111111111111111111111111111111111"
	t.Cleanup(func() { projectsExpectedRegistration = "" })
	projectsRemoveJSON = true
	t.Cleanup(func() { projectsRemoveJSON = false })
	stdout := &bytes.Buffer{}
	projectsRemoveCmd.SetOut(stdout)

	require.NoError(t, runProjectsRemove(
		projectsRemoveCmd,
		[]string{"/code/widget"},
	))

	var response projectMutationResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	assert.Equal(t, "github.com/acme/widget", response.Project.Repository)
	assert.NotContains(t, stdout.String(), token)
}

func TestRunProjectsRemoveJSONReportsMissingProject(t *testing.T) {
	originalRemove := removeProjectThroughDaemon
	t.Cleanup(func() { removeProjectThroughDaemon = originalRemove })
	removeProjectThroughDaemon = func(context.Context, kwt.ProjectRemovalRequest) (kwt.ProjectRemovalResult, error) {
		return kwt.ProjectRemovalResult{}, service.NewError(
			service.ProjectNotFound, "no project is registered at the exact path", false, nil, nil,
		)
	}
	projectsExpectedRepository = "github.com/acme/widget"
	t.Cleanup(func() { projectsExpectedRepository = "" })
	projectsExpectedRegistration = "v1:1111111111111111111111111111111111111111111111111111111111111111"
	t.Cleanup(func() { projectsExpectedRegistration = "" })
	projectsRemoveJSON = true
	t.Cleanup(func() { projectsRemoveJSON = false })
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	projectsRemoveCmd.SetOut(stdout)
	projectsRemoveCmd.SetErr(stderr)

	err := runProjectsRemove(projectsRemoveCmd, []string{"/missing/widget"})

	var exitErr interface{ ExitCode() int }
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 2, exitErr.ExitCode())
	var response jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	assert.Equal(t, service.Code("project_not_found"), response.Error.Code)
	assert.False(t, response.Error.Retryable)
	assert.Contains(t, stderr.String(), "project_not_found")
}

func TestRunProjectsRemoveHumanResolvesCurrentExactRegistration(t *testing.T) {
	originalQuery := queryProjectsInventory
	originalRemove := removeProjectThroughDaemon
	t.Cleanup(func() {
		queryProjectsInventory = originalQuery
		removeProjectThroughDaemon = originalRemove
		projectsExpectedRepository = ""
		projectsExpectedRegistration = ""
		projectsRemoveJSON = false
	})
	queryProjectsInventory = func(
		_ context.Context,
		request kwt.Request,
		_ bool,
		_ io.Writer,
	) (kwt.Result, error) {
		assert.True(t, request.RequireCurrent)
		return kwt.Result{Snapshot: kwt.Snapshot{Projects: []kwt.Project{
			{Path: "/repo", Repository: "github.com/acme/other", RegistrationFingerprint: "v1:2222222222222222222222222222222222222222222222222222222222222222"},
			{Path: "/repo ", Repository: "github.com/acme/widget", RegistrationFingerprint: "v1:3333333333333333333333333333333333333333333333333333333333333333"},
		}}}, nil
	}
	var requested kwt.ProjectRemovalRequest
	removeProjectThroughDaemon = func(
		_ context.Context,
		request kwt.ProjectRemovalRequest,
	) (kwt.ProjectRemovalResult, error) {
		requested = request
		return kwt.ProjectRemovalResult{Project: models.Project{
			Path: request.Path, Repository: request.ExpectedRepository, Name: "widget",
		}}, nil
	}
	stdout := &bytes.Buffer{}
	projectsRemoveCmd.SetOut(stdout)

	require.NoError(t, runProjectsRemove(projectsRemoveCmd, []string{"/repo "}))

	assert.Equal(t, "/repo ", requested.Path)
	assert.Equal(t, "github.com/acme/widget", requested.ExpectedRepository)
	assert.Equal(t, "v1:3333333333333333333333333333333333333333333333333333333333333333", requested.ExpectedRegistration)
	assert.Contains(t, stdout.String(), "unregistered project")
}

func TestRunProjectsRemoveRejectsPartialExpectedRegistration(t *testing.T) {
	t.Cleanup(func() {
		projectsExpectedRepository = ""
		projectsExpectedRegistration = ""
		projectsRemoveJSON = false
	})
	projectsExpectedRepository = "github.com/acme/widget"
	stdout := &bytes.Buffer{}
	projectsRemoveCmd.SetOut(stdout)

	err := runProjectsRemove(projectsRemoveCmd, []string{"/repo"})

	assert.True(t, service.IsCode(err, service.InvalidRequest))
}

func TestProjectsAddArgumentValidationUsesStructuredErrors(t *testing.T) {
	projectsAddJSON = true
	t.Cleanup(func() { projectsAddJSON = false })
	cmd, stdout, stderr := projectsTestCommand()

	err := projectsExactArgs(1)(cmd, nil)

	var exitErr *commandFailure
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 2, exitErr.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.Code("invalid_repository"), envelope.Error.Code)
	assert.Contains(t, stderr.String(), "invalid_repository")
}

func TestProjectsAddUnknownFlagBeforeJSONUsesStructuredError(t *testing.T) {
	if os.Getenv("KWT_TEST_PROJECTS_UNKNOWN_FLAG") == "1" {
		os.Args = []string{
			os.Args[0],
			"projects",
			"add",
			"--bogus",
			"--json",
		}
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		Execute()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestProjectsAddUnknownFlagBeforeJSONUsesStructuredError$")
	cmd.Env = append(os.Environ(), "KWT_TEST_PROJECTS_UNKNOWN_FLAG=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 2, exitErr.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.Code("invalid_repository"), envelope.Error.Code)
	assert.Contains(t, stderr.String(), "invalid_repository")
}

func TestProjectsConfigInitializationFailureUsesJSONContract(t *testing.T) {
	if os.Getenv("KWT_TEST_PROJECTS_CONFIG_INIT_FAILURE") == "1" {
		rootCmd.SetArgs([]string{"projects", "add", "/missing", "--json"})
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		Execute()
		return
	}

	kwtHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte("invalid = [\n"), 0o600))
	cmd := exec.Command(os.Args[0], "-test.run=^TestProjectsConfigInitializationFailureUsesJSONContract$")
	cmd.Env = append(os.Environ(),
		"KWT_TEST_PROJECTS_CONFIG_INIT_FAILURE=1",
		"KWT_HOME="+kwtHome,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.Code("registration_failed"), envelope.Error.Code)
	assert.Contains(t, stderr.String(), "registration_failed")
}

func projectsTestCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd, stdout, stderr
}
