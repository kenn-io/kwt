# Validate Imported Worktree Merge Conflicts Against Kit PR 77

## Goal

Ensure kwt consumes the kit change for kwt#112 before kit is released, and
prove that a merge or rebase in an imported worktree preserves conflict
information instead of leaving a current-side-only file marked `UU`.

## Scope

The merge-driver implementation belongs to kit PR #77. The kwt change will
consume that unreleased kit tip, exercise it through kwt's real pull-request
backend, and document the resulting Git version requirement. No duplicate
merge-driver implementation will be added to kwt.

## Design

Update `go.kenn.io/kit` temporarily to the pseudo-version for kit PR #77 tip
`baf69924c6cb25d6235e59fe98e6c46780cd387d`:
`v0.22.1-0.20260831205235-baf69924c6cb`. This pseudo-version is for pre-release
validation only. Because the PR will likely be squash-merged, its tip may not
remain reachable from kit's main history. After kit is merged and released as
`v0.23.0`, replace the pseudo-version with that released tag before the kwt PR
merges, and rerun the focused and full checks.

Add an integration regression test in the pull-request backend test package.
The test will use temporary local Git repositories for the trusted project and
pull-request source, configure a tracked file to select a custom merge driver
that returns failure, and invoke `GitBackend.ImportPullRequest` with the real
kit lifecycle function. It will then perform a conflicting merge in the
imported worktree and inspect the actual Git status and file contents.

The fixture will keep the public GitHub identity used by `Project.Identity` and
the pull-request metadata so kwt's post-import push validation still runs. Its
trusted project's `origin` will point to the temporary local repository for the
same-repository fetch, while an explicit canonical GitHub-shaped `pushurl`
keeps kwt's push validation on the trusted identity. The pull-request head
clone URL will use that canonical URL, so kit's same-repository identity
comparison and kwt's normal tracking setup are exercised without a network
request. The complete `internal/pullrequest` package is exercised in Linux CI;
Windows CI does not select it, so no platform-specific skip is needed for this
test.

The test must observe all of the issue's important user-visible behavior:

- the imported worktree is created through kwt's production backend wiring;
- the conflicting path remains unmerged (`UU`);
- the working file contains diff3 conflict markers; and
- both current and other changes remain visible in that file.

The test fixture will isolate Git configuration and use only temporary local
repositories. It will not contact GitHub or depend on the developer's global
Git configuration. Existing test helpers and the repository test harness will
remain the source of process and environment isolation.

Update the user-facing requirements in exactly these files: `README.md`,
`docs/get-started/install.md`, `docs/get-started/quickstart.md`,
`docs/reference/cli.md`, `docs/reference/pull-requests.md`, and
`docs/changelog.md`. Keep the general Git 2.20 floor and the Git 2.31 floor for
doctor and prune policies, and add a third exception for pull-request imports:
Git 2.42.0 or newer on macOS and Linux, or Git for Windows
2.53.0.windows.3 or newer. The pull-request reference must state this beside
the per-worktree configuration explanation, since that paragraph otherwise
promises support for the affected feature on Git 2.20. Add the same import
floor to the `kwt pr`/`kwt pr import` help text in `internal/cmd/pr.go`, and add
an Unreleased changelog entry describing preserved conflict contents and the
higher import floor.

The regression test will use a conflicting `git merge`, rather than a rebase.
Both operations invoke Git's configured merge driver for the conflicted path;
the merge gives the test a direct, deterministic assertion of the same
current-side-only failure that the issue reports during rebase.

## Verification

Use the focused backend test during development, with the failing test run
against kit v0.22.0 before dependency wiring is complete. Then update to the
kit PR pseudo-version and verify that the same test passes. Before the kwt PR
merges, replace that pseudo-version with kit v0.23.0 and rerun the focused test,
`make test`, the repository build, lint, and `make docs-check`. `make docs-check`
only proves that the documentation site builds; manually review every named
version statement to catch a missed or inaccurate requirement. Review the
final diff and staged contents before committing.

## Non-goals

- Do not reimplement kit's safe merge-driver command in kwt.
- Do not add a compatibility fallback for Git versions below the new
  pull-request import floor.
- Do not merge kit PR #77, tag kit, push branches, or post GitHub comments.
