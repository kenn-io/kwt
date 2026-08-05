# Inaccessible Project Listing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `kwt projects` emit only registered projects whose configured paths are accessible Git repositories.

**Architecture:** Keep filtering at the existing emission-time `canonicalizeProjectIdentities` boundary shared by JSON and table output. Validate each project through `GetMainRepositoryPath`, reuse a cached Git identity surface for canonical identity resolution, and preserve the on-disk registry unchanged.

**Tech Stack:** Go, Cobra, Testify, kwt's `internal/git` and `internal/worktree` packages.

## Global Constraints

- Omit missing, inaccessible, non-Git, and path-less registry entries from both output formats.
- Continue emitting accessible entries and exit successfully when other entries are omitted.
- Emit `[]`, not `null`, when filtering removes every JSON entry.
- Do not scan for moved repositories or mutate persisted registry metadata.
- Remove the obsolete identity fallbacks and the test that pinned path-less output.
- Use test-first development and keep tests focused on observable command behavior.

---

### Task 1: Filter project listings at the shared identity boundary

**Files:**
- Modify: `internal/cmd/projects_test.go`
- Modify: `internal/cmd/projects.go`
- Modify: `docs/reference/cli.md`

**Interfaces:**
- Consumes: `git.New(path string) *git.Git`, `worktree.NewCachedIdentityGit(g worktree.RepoIdentityGit) *worktree.CachedIdentityGit`, and `worktree.RepositoryInfoWithProjects(g worktree.RepoIdentityGit, projects []models.Project) (*url.RepositoryInfo, error)`.
- Produces: `canonicalizeProjectIdentities(projects []models.Project) []models.Project`, now returning a non-nil slice containing only accessible projects with canonical repository identities.

- [ ] **Step 1: Repair existing test fixtures and write failing regression coverage**

Update `TestRunProjectsJSONEmitsRegistry` and `TestRunProjectsRendersTable` to create `repoPath := newTUITestRepo(t)`, use `repoPath` as the registered path, and assert that emitted output contains that path instead of `/home/wesm/code/kwt`.

Replace `TestRunProjectsFallsBackToStoredLocalIdentityWithoutPath` with:

```go
func TestRunProjectsOmitsInaccessibleRegistryEntries(t *testing.T) {
	livePath := newTUITestRepo(t)
	missingPath := filepath.Join(t.TempDir(), "missing")
	withProjectsConfig(t, []models.Project{
		{Repository: "github.com/example/live", Name: "live", Path: livePath},
		{Repository: "github.com/example/missing", Name: "missing", Path: missingPath},
		{Repository: "local/pathless", Name: "pathless"},
	})

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "live", got[0].Name)
	assert.Equal(t, livePath, got[0].Path)
}
```

Strengthen `TestRunProjectsJSONEmptyIsArray` with `require.NotNil(t, got)`, then add the fully filtered case:

```go
func TestRunProjectsJSONFullyFilteredIsArray(t *testing.T) {
	withProjectsConfig(t, []models.Project{
		{
			Repository: "github.com/example/missing",
			Name:       "missing",
			Path:       filepath.Join(t.TempDir(), "missing"),
		},
		{Repository: "local/pathless", Name: "pathless"},
	})

	out := runProjectsForTest(t, true)

	var got []models.Project
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotNil(t, got)
	assert.Empty(t, got)
}
```

- [ ] **Step 2: Run the focused tests and verify the new regression fails**

Run:

```bash
go test ./internal/cmd -run 'TestRunProjects(JSONEmitsRegistry|RendersTable|OmitsInaccessibleRegistryEntries|JSONEmptyIsArray|JSONFullyFilteredIsArray)$' -count=1
```

Expected: `TestRunProjectsOmitsInaccessibleRegistryEntries` and `TestRunProjectsJSONFullyFilteredIsArray` fail because inaccessible and path-less entries are still emitted. The repaired existing fixtures pass.

- [ ] **Step 3: Implement the minimal shared filter and delete dead fallbacks**

Remove the unused `go.kenn.io/kwt/internal/url` import and replace `canonicalizeProjectIdentities` plus `publishableProjectRepository` with:

```go
// canonicalizeProjectIdentities returns accessible registered projects with
// Repository values resolved through the canonical identity bar, so projects
// output (JSON and table) emits the same identities kwt list --json reports.
func canonicalizeProjectIdentities(projects []models.Project) []models.Project {
	out := make([]models.Project, 0, len(projects))
	for _, project := range projects {
		repositoryGit := worktree.NewCachedIdentityGit(git.New(project.Path))
		if _, err := repositoryGit.GetMainRepositoryPath(); err != nil {
			continue
		}
		info, err := worktree.RepositoryInfoWithProjects(
			repositoryGit,
			[]models.Project{project},
		)
		if err != nil {
			continue
		}
		project.Repository = info.FullPath
		out = append(out, project)
	}
	return out
}
```

This preserves a non-nil empty slice and caches the successful repository-root lookup for identity resolution. Do not add retries or synchronization for a repository moved concurrently with the listing.

- [ ] **Step 4: Document the project-list accessibility contract**

In `docs/reference/cli.md` under `kwt projects`, add this paragraph after the JSON identity description:

```markdown
Entries whose configured paths are missing, inaccessible, not Git repositories,
or empty are omitted from both table and JSON output. Kwt does not delete those
registry records or scan for moved checkouts; running kwt in the checkout's new
location, or using `kwt projects add <new-path>`, updates the existing record by
repository identity.
```

- [ ] **Step 5: Format and verify the focused behavior passes**

Run:

```bash
gofmt -w internal/cmd/projects.go internal/cmd/projects_test.go
go test ./internal/cmd -run 'TestRunProjects' -count=1
```

Expected: all `TestRunProjects...` tests pass.

- [ ] **Step 6: Run repository verification**

Run:

```bash
make test
make build
make docs-build
git diff --check
```

Expected: all commands exit successfully with no formatting errors.

- [ ] **Step 7: Commit the implementation**

Review and stage only `internal/cmd/projects.go`, `internal/cmd/projects_test.go`, and `docs/reference/cli.md`. Use the mandatory `kenn:commit` workflow and create a rationale-first commit such as `Ignore inaccessible registered projects` without amending either design commit.

### Task 2: Repair and verify the current local registry

**Files:**
- Modify outside the repository: kwt's user-level project registry through the supported CLI.

**Interfaces:**
- Consumes: `kwt projects add <path> --json` and `kwt projects --json`.
- Produces: the existing `github.com/kenn-io/middleman` registry entry updated from `/Users/wesm/code/middleman` to `/Users/wesm/code/forge`.

- [ ] **Step 1: Register the renamed checkout through the supported boundary**

Run:

```bash
/Applications/Ghosthub.app/Contents/Helpers/kwt projects add /Users/wesm/code/forge --json
```

Expected: a successful `registered` response for repository identity `github.com/kenn-io/middleman` with path `/Users/wesm/code/forge`.

- [ ] **Step 2: Verify the stale path is gone from local project output**

Run:

```bash
/Applications/Ghosthub.app/Contents/Helpers/kwt projects --json
```

Expected: the Middleman entry has path `/Users/wesm/code/forge`, and no entry has path `/Users/wesm/code/middleman`.

- [ ] **Step 3: Close the tracked issue after verified implementation**

After the implementation commit exists, capture it and close the issue:

```bash
kwt_implementation_sha=$(git rev-parse HEAD)
kata close x4kp --done --message "Project listings now omit inaccessible registry entries while preserving metadata for later re-registration; focused and full tests plus build pass, and the renamed local checkout is registered at /Users/wesm/code/forge." --commit "$kwt_implementation_sha"
```
