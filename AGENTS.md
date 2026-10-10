# kwt — agent guidance

## Agent rules

- Always Commit: Do not leave accepted repository changes uncommitted at the end of a task. Commit the completed work, or explicitly say why no commit was made.
- Never Squash or Amend: Do not squash commits, amend commits, or otherwise rewrite git history unless the user explicitly asks for that history rewrite.
- Do not commit rejected experiments. Revert them or ask before preserving them.
- Test First: Write a failing test before implementation, then make it pass, then refactor. Do not add production code without a failing test that requires it.
- No Unrequested GitHub Comments: Do not comment on GitHub issues or pull requests unless the user explicitly instructs you to post a comment.
- User or Developer Benefit: Pull requests must have a user-facing benefit or improve the developer experience, and the body must say which one.
- No CI Polling: Do not poll GitHub or the `gh` API to watch jobs or workflow status unless the user explicitly instructs you to do so.
- No Navel-Gazing Validation Sections: Do not add a `Validation` section to a PR description for routine tests, builds, lint, formatting, or CI. Include one only when the validation was unusual, potentially surprising, manual, or otherwise important for reviewers to understand.
- No Bash Content-Assertion Tests: Do not add shell tests that only grep scripts, workflows, or config files for implementation text. Prefer exercising behavior directly or documenting a manual check.
- Documentation should move with behavior changes when practical: CLI flags, config keys, workflows, and user-facing contracts should be updated with the code.
- Keep changes focused. Do not refactor unrelated code or rewrite user changes while completing a task.
- Prefer the repo's commands for verification: `make test`, `make build`, and focused `go test ./path` runs while iterating.

## Worktree lifecycle

CLI, TUI, and fleet creation use `internal/worktree.Manager.Create`, which
returns the public `worktree.CreateResult`. Keep naming, provenance records,
and warning-only setup in the application. Use the captured identity for
registry writes and retain the result for rollback; do not reconstruct cleanup
authority from a path or branch name.

The default-branch fetch runs before creation synchronization because native
Git hooks may re-enter inventory. Plain checkout uses the shared creation
reservation, releasing the mutation lock while hooks run. Existing and remote
branches use Kit's isolated checkout through a held shared scope.

PR imports hold project claim, registry path lease, provenance lock, then
repository mutation lock, in that order. Listing uses a short scope before
import. Creation, identity finalization, and immediate rollback share the
import scope; a failed provenance save reacquires the repository lock for
conservative cleanup and must respect an active plain-creation reservation.

Inventory, generation guards, removal, and maintenance use the public
`worktree` package. `internal/git.WorktreeRepository` supplies application
execution policy and the shared coordinator; it owns no lifecycle mechanics.
Keep app JSON models as projections of inventory and pass a held `Scope` to
callbacks that need more Git facts. `PrimaryPath` is an advisory path lookup
that neither acquires the mutation lock nor initializes generation files.

Removal claims receive cleanup effects before committing registry changes.
A removed registration permits registry cleanup even if checkout files remain;
return the cleanup error afterward. Direct removal without a session guard
leaves native dirty/submodule refusal to Git after the process check. Session
removal preflights before stopping a runtime and revalidates before cleanup.

Warm claims retain Kit acquisition evidence for the consumed checkout and any
new branch. Keep that result for rollback; a preexisting branch remains outside
cleanup authority, and an unknown move outcome must preserve its artifacts.

## CI runners

Public CI profiles use Namespace's
[Restricted access level](https://namespace.so/docs/solutions/github-actions/runner-controls/access-levels),
which disables workload access to Namespace features and APIs. GitHub fork
approvals, token permissions, and secrets are separate controls.

<!-- BEGIN KATA (managed by `kata init --with-agents`) -->

## kata issue tracker

This project uses [kata](https://github.com/kenn-io/kata) as its shared issue
ledger. Run `kata quickstart` at the start of each session for the full agent
contract. The short version:

- Search before creating: `kata search "<keywords>" --agent`.
- Prefer updating existing issues over duplicates (`kata comment`, `kata label add`, `kata edit`).
- Default to `--agent` for ordinary reads and mutations; use `--json` only when a script needs structured data.
- Close only verified work: `kata close <ref> --done --message "<scope + verification>" --commit <sha>`.
- If work is incomplete, label `needs-review` and comment what remains rather than closing.
- Never `kata delete` or `kata purge` without explicit user authorization.

<!-- END KATA -->

Repository inventory may be opened through a normal checkout's common `.git`
directory. Resolve its primary checkout before querying worktree-only facts.
For a recorded path replaced by another repository, use the held scope's
`InspectRegistration` and `PruneRegistration`: stale cleanup removes only that
administrative entry, preserving replacement files and unrelated registrations.
Symlink paths never authorize registration cleanup.

Use `ObserveRegistration` for advisory repository selection before acquiring a
scope. It reads an optional marker without creating one or taking another lock;
cleanup must still revalidate through the held scope. `Quarantine` preserves
orphaned files under a caller-selected name and leaves valid checkout roots
alone. Branch cleanup after stale registration removal uses `Scope.RemoveBranch`
so checked-out branches and explicit expected revisions remain enforced by Kit.
