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

Update `go.kenn.io/kit` to the pseudo-version for kit PR #77 tip
`baf69924c6cb25d6235e59fe98e6c46780cd387d`. Add an integration regression test
in the pull-request backend test package. The test will use temporary local Git
repositories for the trusted project and pull-request source, configure a
tracked file to select a custom merge driver that returns failure, and invoke
`GitBackend.ImportPullRequest` with the real kit lifecycle function. It will
then perform a conflicting merge in the imported worktree and inspect the
actual Git status and file contents.

The test must observe all of the issue's important user-visible behavior:

- the imported worktree is created through kwt's production backend wiring;
- the conflicting path remains unmerged (`UU`);
- the working file contains diff3 conflict markers; and
- both current and other changes remain visible in that file.

The test fixture will isolate Git configuration and use only temporary local
repositories. It will not contact GitHub or depend on the developer's global
Git configuration. Existing test helpers and the repository test harness will
remain the source of process and environment isolation.

Update the user-facing requirements so the general Git 2.20 floor remains,
Git 2.31 remains required for doctor and prune policies, and pull-request
imports are called out separately as requiring Git 2.42.0 or newer on macOS and
Linux. The Git for Windows floor from kit's contract will be documented with
the pull-request import requirement. Add an Unreleased changelog entry that
describes preserved conflict contents and the higher import floor.

## Verification

Use the focused backend test during development, with the failing test run
before implementation or dependency wiring is complete. Then run the full
repository test suite through `make test`, the repository build, lint, and the
documentation checks that cover the changed requirements. Review the final
diff and staged contents before committing.

## Non-goals

- Do not reimplement kit's safe merge-driver command in kwt.
- Do not add a compatibility fallback for Git versions below the new
  pull-request import floor.
- Do not merge kit PR #77, tag kit, push branches, or post GitHub comments.
