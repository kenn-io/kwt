# kwt — agent guidance

## Agent rules

- Always Commit: Do not leave accepted repository changes uncommitted at the end of a task. Commit the completed work, or explicitly say why no commit was made.
- Never Squash or Amend: Do not squash commits, amend commits, or otherwise rewrite git history unless the user explicitly asks for that history rewrite.
- Do not commit rejected experiments. Revert them or ask before preserving them.
- Test First: Write a failing test before implementation, then make it pass, then refactor. Do not add production code without a failing test that requires it.
- No Unrequested GitHub Comments: Do not comment on GitHub issues or pull requests unless the user explicitly instructs you to post a comment.
- No CI Polling: Do not poll GitHub or the `gh` API to watch jobs or workflow status unless the user explicitly instructs you to do so.
- No Navel-Gazing Validation Sections: Do not add a `Validation` section to a PR description for routine tests, builds, lint, formatting, or CI. Include one only when the validation was unusual, potentially surprising, manual, or otherwise important for reviewers to understand.
- No Bash Content-Assertion Tests: Do not add shell tests that only grep scripts, workflows, or config files for implementation text. Prefer exercising behavior directly or documenting a manual check.
- Documentation should move with behavior changes when practical: CLI flags, config keys, workflows, and user-facing contracts should be updated with the code.
- Keep changes focused. Do not refactor unrelated code or rewrite user changes while completing a task.
- Prefer the repo's commands for verification: `make test`, `make build`, and focused `go test ./path` runs while iterating.

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
