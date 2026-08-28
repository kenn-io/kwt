# Worktree lifecycle and maintenance

Use kwt's everyday commands to create, inspect, diagnose, and remove worktrees.
The inspection commands are read-only. Commands that can remove data show the
exact target or support a preview before they change anything.

## Create or import a worktree

Create a new branch and worktree together:

```sh
kwt add -b feature/new-ui
```

Create a worktree from a branch that already exists:

```sh
kwt add --from origin/feature/review feature/review
```

Existing-branch worktrees start inert. kwt skips repository setup and workspace
launch until you inspect the checkout and open it explicitly:

```sh
kwt changes "$(kwt get feature/review)"
kwt open "$(kwt get feature/review)"
```

`kwt pr import` creates the same kind of inert checkout for a GitHub pull
request, with extra push-routing and protected-session rules. See
[Pull-request automation](../reference/pull-requests.md) before automating that
workflow.

## Inspect current state

Use `kwt status` for a cross-project summary of branch, dirty, ahead, behind,
activity, and optional multi-machine state:

```sh
kwt status
```

Use `kwt changes` when you need the exact changed files in one registered
worktree:

```sh
kwt changes /path/to/worktree
kwt changes /path/to/worktree --json
```

The result separates staged and working-tree changes and includes untracked
files, renames, copies, deletions, and conflicts. kwt verifies that it inspected
the same worktree generation from start to finish instead of returning a stale
result for a replacement checkout.

Both commands are read-only. Use `kwt list --json` when automation needs the
current worktree inventory as well as status details.

## Diagnose structural problems

Run doctor when a repository moved, a directory was deleted outside kwt, or Git
and kwt disagree about a worktree:

```sh
kwt doctor
```

Doctor reports broken Git backlinks, stale metadata, and project or registry
drift without changing anything. Review the **Ready to fix** and **Needs
review** groups before applying repairs:

```sh
kwt doctor --fix
```

`--fix` repairs only findings that kwt can prove have one safe answer. It then
scans again and reports anything that still needs a person to choose. Use
`--json` for a versioned report or `--quiet` when a script needs only the exit
status. Doctor requires Git 2.31 or newer.

## Remove merged or expired worktrees

Prune requires one explicit policy and Git 2.31 or newer. Preview the complete
decision set first:

```sh
kwt prune --expired --dry-run
kwt prune --merged --dry-run
```

Then run the policy without `--dry-run` to remove eligible worktrees:

```sh
kwt prune --expired
kwt prune --merged
```

Expiration pruning applies only to live worktrees whose configured expiration
has passed. `--force` can remove a dirty expired worktree, but it does not bypass
kwt's identity checks.

Merged pruning requires an exact GitHub pull request with a confirmed merged
result. It preserves the local branch. In an interactive terminal, kwt asks
before removing a confirmed merged worktree that still has files or changes;
the prompt defaults to no. JSON and other unattended runs report
`confirmation_required` instead of prompting. Merged pruning has no `--force`
mode.

Bare `kwt prune` is a usage error. Structural cleanup belongs to
`kwt doctor --fix`, not to an implicit prune policy.

## Remove one worktree directly

Preview one direct removal when you want to confirm pattern matching:

```sh
kwt remove --dry-run feature/old
```

Remove only the worktree, or remove its local branch too:

```sh
kwt remove feature/old
kwt remove -b feature/old
```

Direct removal refuses a dirty worktree and a worktree used as the current
directory of a live process. The error lists visible blocking process IDs.
`--force` bypasses those two checks; inspect the worktree and stop the process
before using it. Deleting an unmerged branch is a separate decision and
requires `--force-delete-branch` with `-b`.

Automation can bind removal to the `generation` and tmux endpoint returned by
current JSON inventory. See [`kwt remove`](../reference/cli.md#kwt-remove) for
those guarded flags.

## Understand safety stops

kwt reports a specific outcome when it declines or cannot fully finish a
mutation:

- `doctor_required` means the path or metadata needs structural inspection.
  This commonly appears when a prune candidate's directory is already absent.
  Run `kwt doctor`, review its evidence, and use `kwt doctor --fix` only for the
  findings marked safe to repair.
- `cleanup_incomplete` means Git removed the worktree from its inventory, but
  files remain at the reported path. kwt completes guarded registry, session,
  and publication bookkeeping. Inspect and remove only the residual files you
  recognize.
- A dirty-worktree result preserves local changes. Commit, move, or discard
  them before retrying, unless the documented force or confirmation path is
  what you intend.
- A live-process result protects a shell, agent, or other process whose current
  directory is inside the worktree. Stop the listed process before retrying.
- A stale-generation, registration, or session result means the workspace
  changed after it was displayed. Refresh inventory and make a new decision
  instead of retrying with the old guard values.

For exhaustive outcomes, JSON fields, and exit statuses, use the
[CLI reference](../reference/cli.md).
