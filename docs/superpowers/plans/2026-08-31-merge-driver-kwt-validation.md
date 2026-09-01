# Imported Worktree Merge-Driver Validation Implementation Plan

> For agentic workers: REQUIRED SUB-SKILL: Use superpowers:executing-plans (or superpowers:subagent-driven-development) to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Prove kwt preserves both sides of a merge conflict in an imported pull-request worktree while consuming kit PR #77, then publish the matching Git-version requirements.

**Architecture:** Keep the merge-driver implementation in kit. Add one kwt integration regression test that calls GitBackend.ImportPullRequest without replacing its createMergeRequestWorktree seam, then runs a real conflicting Git merge in the returned worktree. Use a repository-local URL rewrite so the fixture retains the canonical GitHub identity required by kwt and kit while all Git objects remain in temporary local repositories.

**Tech Stack:** Go 1.27, Go modules, Git temporary repositories, testify, Cobra help text, Markdown/Zensical documentation, and the repository make targets.

## Global Constraints

- Kit PR #77 was merged at 6bef84f5bf0b2400a04f37c8f1284a6eef12b2a8 and released as go.kenn.io/kit v0.23.0.
- Keep kwt on the released kit tag and rerun the focused and full checks before the kwt PR merges.
- Preserve kwt's general Git 2.20 floor and its Git 2.31 floor for kwt doctor and prune policies.
- Document the separate pull-request import floor: Git 2.42.0 or newer on macOS/Linux, or Git for Windows 2.53.0.windows.3 or newer.
- Do not duplicate kit's merge-driver implementation or add a compatibility path for older Git versions.
- Keep the integration fixture local, temporary, non-networked, and independent of global Git configuration.
- Do not add a Windows skip solely for this test; Windows CI does not select the complete internal/pullrequest package.

---

### Task 1: Add the failing kwt integration regression test

**Files:**
- Modify: internal/pullrequest/backend_test.go

**Interfaces:**
- Consumes: existing newBackendRepo, runGit, testPR, GitBackend.ImportPullRequest, and the real createMergeRequestWorktree package variable.
- Produces: a regression test named TestGitBackendImportedWorktreePreservesMergeConflictContents that fails on kit v0.22.0 because the imported conflict file has no markers and lacks one side's content.

- [ ] **Step 1: Write the failing test.**

Add the test after TestGitBackendDelegatesPullRequestLifecycleToKit. It must not override createMergeRequestWorktree:

~~~go
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
~~~

Keep Project.Identity and the pull-request metadata canonical as github.com/acme/widget. Point the trusted project's fetch origin at the temporary local repository and set its pushurl to the canonical GitHub URL. This exercises kit's same-repository identity comparison and kwt's post-import push validation without GitHub access.

- [ ] **Step 2: Run the focused test and verify RED.**

Run:

~~~sh
make test TEST_PACKAGES=./internal/pullrequest
~~~

Expected result with go.kenn.io/kit v0.22.0: the new test reaches the merge, Git reports UU conflict.txt, and an assertion fails because the old replacement driver leaves missing conflict contents. The failure must not be a missing executable, Git-version rejection, network request, or fixture identity error. If it fails earlier, fix only the fixture until the old kit behavior is reproduced.

- [ ] **Step 3: Commit the red regression test.**

Review git status --short, git diff --stat, and git diff --cached --check. Stage only internal/pullrequest/backend_test.go and commit with this message, followed by the required Codex attribution block:

~~~text
test: reproduce unmarked imported merge conflicts

Imported pull-request worktrees previously reported an unmerged path while
leaving only the current side in the working file. Exercise the real kwt
backend so the missing conflict contents fail before the kit repair is used.
~~~

### Task 2: Consume kit PR #77 and turn the regression green

**Files:**
- Modify: go.mod
- Modify: go.sum
- Test: internal/pullrequest/backend_test.go

**Interfaces:**
- Consumes: the failing integration test from Task 1.
- Produces: kwt builds against released kit v0.23.0, with the same test passing through the real kit lifecycle.

- [ ] **Step 1: Update the Go module using Go tooling.**

Run:

~~~sh
go get go.kenn.io/kit@v0.23.0
go mod verify
~~~

Confirm go.mod records exactly go.kenn.io/kit v0.23.0 and go.sum contains only the matching current module checksum. Do not hand-edit either file or run a broad dependency upgrade.

- [ ] **Step 2: Run the focused test and verify GREEN.**

Run:

~~~sh
make test TEST_PACKAGES=./internal/pullrequest
~~~

Expected result: the imported worktree merge reports UU conflict.txt, and the working file contains current, base, and other diff3 sections plus both branch changes. The test must use the real kit function because no test override is installed.

- [ ] **Step 3: Commit the dependency handoff.**

Review the module diff and staged checks, then commit go.mod and go.sum with this message, followed by the required Codex attribution block:

~~~text
fix: validate imported merge conflicts with kit tip

Use released kit v0.23.0 after validating its merge-driver repair. The
pull-request backend exercises the behavior that preserves both sides of an
imported worktree conflict before kwt is released.
~~~

### Task 3: Publish the new pull-request import requirement

**Files:**
- Modify: README.md
- Modify: docs/get-started/install.md
- Modify: docs/get-started/quickstart.md
- Modify: docs/reference/cli.md
- Modify: docs/reference/pull-requests.md
- Modify: docs/changelog.md
- Modify: internal/cmd/pr.go

**Interfaces:**
- Consumes: kit PR #77's documented platform floors and kwt's existing Git 2.20/2.31 statements.
- Produces: consistent user-facing requirements in all named Markdown surfaces and kwt pr import --help.

- [ ] **Step 1: Update README requirements.**

Change the requirements paragraph to state that Git 2.20 remains the general floor, doctor and both prune policies require Git 2.31, and kwt pr import additionally requires Git 2.42.0 or newer on macOS/Linux or Git for Windows 2.53.0.windows.3 or newer.

- [ ] **Step 2: Update installation and quickstart guidance.**

In docs/get-started/install.md, add the pull-request import exception as its own Git requirement bullet. In docs/get-started/quickstart.md, add the same requirement under “Before you start” so a user following the PR workflow sees it before importing.

- [ ] **Step 3: Update the CLI reference and command help.**

In the opening requirements paragraph of docs/reference/cli.md, add the third exception without changing the general and maintenance floors. Add a Long field to prImportCmd in internal/cmd/pr.go:

~~~go
Long: "Pull-request import requires Git 2.42.0 or newer on macOS and Linux, " +
	"or Git for Windows 2.53.0.windows.3 or newer.",
~~~

Keep the list and attach commands free of the import-only requirement.

- [ ] **Step 4: Correct the pull-request contract paragraph.**

In docs/reference/pull-requests.md, keep the existing Git 2.20 and Git 2.31 statements, then explicitly say that PR import's per-worktree configuration and safe merge-driver behavior require Git 2.42.0 or newer on macOS/Linux or Git for Windows 2.53.0.windows.3 or newer.

- [ ] **Step 5: Add the Unreleased changelog entry.**

Under ## Unreleased in docs/changelog.md, add a user-outcome bullet explaining that imported pull-request worktrees now preserve both sides and diff3 markers for text conflicts, and name the new platform-specific Git floor.

- [ ] **Step 6: Review every version claim.**

Run:

~~~sh
rg -n -C 1 'Git 2\.20|Git 2\.31|Git 2\.42|2\.53\.0\.windows\.3' README.md docs/get-started/install.md docs/get-started/quickstart.md docs/reference/cli.md docs/reference/pull-requests.md docs/changelog.md internal/cmd/pr.go
~~~

Confirm every named file contains the intended exception and no pull-request-specific paragraph still implies that Git 2.20 is sufficient for import.

- [ ] **Step 7: Commit the documentation and help changes.**

Run git diff --check, stage only the seven listed files, and commit with this message, followed by the required Codex attribution block:

~~~text
docs: document pull-request Git version floor

Imported pull-request worktrees now use Git's safe three-way merge behavior,
which requires newer Git versions outside Windows. State the higher floor where
users install, start, inspect, and import pull requests so upgrades do not hide
the requirement.
~~~

### Task 4: Verify the complete kwt-side change

**Files:**
- Test: all repository packages through make test
- Build: make build
- Docs: make docs-check
- Static checks: make vet and make lint

**Interfaces:**
- Consumes: the three implementation commits and released kit v0.23.0.
- Produces: fresh evidence for the regression, full test suite, build, static checks, and docs site against released kit v0.23.0.

- [ ] **Step 1: Run the focused regression once more.**

~~~sh
make test TEST_PACKAGES=./internal/pullrequest
~~~

- [ ] **Step 2: Run the full repository test suite.**

~~~sh
make test
~~~

- [ ] **Step 3: Run build and static checks.**

~~~sh
make build
make vet
make lint
~~~

If golangci-lint is absent, report that as an environment limitation instead of treating the Makefile's informational message as a lint pass.

- [ ] **Step 4: Build the documentation site.**

~~~sh
make docs-check
~~~

This proves the site builds. It does not validate the truth of the version claims, so manually recheck the rg output from Task 3.

- [ ] **Step 5: Review the final diff and public surfaces.**

Inspect:

~~~sh
git status --short
git diff origin/main...HEAD --stat
git diff origin/main...HEAD --check
git diff origin/main...HEAD -- go.mod go.sum internal/pullrequest/backend_test.go README.md docs/get-started/install.md docs/get-started/quickstart.md docs/reference/cli.md docs/reference/pull-requests.md docs/changelog.md internal/cmd/pr.go
~~~

Run the public-artifact scrub over all changed tests, docs, module files, and commit text before any push or PR creation. Ensure no global Git path, home path, credential, private hostname, or developer email entered the fixture or documentation.

- [ ] **Step 6: Record the release handoff.**

Kit PR #77 is released as v0.23.0. Keep the branch on that tag and run:

~~~sh
go get go.kenn.io/kit@v0.23.0
go mod verify
make test TEST_PACKAGES=./internal/pullrequest
make test
make build
make vet
make lint
make docs-check
~~~

Review go.mod and go.sum to confirm the pseudo-version is gone before the kwt PR merges. Do not merge kwt or publish its release as part of this task.

- [ ] **Step 7: Check commit state.**

Run git status --short --branch and git log --oneline origin/main..HEAD. The accepted kwt changes must be committed, with no untracked test binaries or uncommitted source/docs changes.
