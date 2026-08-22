# TUI and Project Registry

The dashboard is the primary `kwt` surface. It should let a user steer worktrees
across repositories without first changing directories.

## Cross-project model

`kwt` keeps a global project registry in the config file under
`~/.config/kwt/config.toml`, or `$KWT_HOME/config.toml` when `KWT_HOME` is set.
The registry records repositories `kwt` has seen:

- stable repository identity, such as `github.com/kenn-io/kwt`;
- display name;
- local repository root path for this host;
- last touched timestamp.

This registry is discovery metadata only. It is deliberately separate from
`repository_settings`, because repository settings change how worktrees are
created.

Dashboard discovery merges three sources:

- worktrees under `worktree.basedir`;
- worktrees reported by every registered project;
- the repository from which the dashboard was launched.

Duplicate paths are de-duplicated. Missing or stale registered paths are ignored
so removing a local clone does not break the dashboard.

## Project filters and perspectives

Lowercase `p` is a project-name filter. It narrows visible rows, but it is still
a filter.

Uppercase `P` selects a project perspective. The active perspective is either
`all` or one project. It filters rows and also becomes the project context for
actions such as `n` new worktree. That distinction lets a user launch `kwt`,
press `P`, choose a project, press `n`, and create the worktree in that project
without exiting the TUI.

Filtering order is:

1. active project perspective;
2. lowercase project-name filter;
3. text search with `/`.

If the active project has no rows after refresh, the dashboard should keep the
perspective and render an empty state instead of silently changing context.

## Inventory refresh and freshness

The dashboard separates inventory currency from Git status collection. It uses
four load shapes:

- cached dashboard inventory for the first paint;
- current repository inventory with status for the active project;
- one current dashboard refresh without status to update the shared cache; and
- current dashboard inventory with status when the user selects all projects.

A repository refresh runs non-interactively. Kwt rejects a result unless it
matches the requested repository identity and contains the expected main
worktree and launch anchor. This prevents the lifecycle service's global
fallback from being accepted as a project result. Accepted repository rows
update the cached dashboard rows instead of replacing them, which preserves
dashboard-only metadata such as the canonical repository URL.

Freshness is scoped to the evidence collected. A repository refresh marks only
that project current. A status-bearing dashboard refresh marks each worktree
project in its result current. The status-free background refresh updates
structural catalog data but never authorizes worktree mutation. Directory
workspaces have no repository status and continue to use global freshness.

Inventory commands carry an increasing request sequence. Starting a newer
request supersedes every older request, and the model discards any older result
that arrives later. Structural dashboard merges retain a newer project status
only while the worktree checkout and generation still match. Every inventory
merge reapplies in-progress creation and removal state before rendering. These
rules prevent a slow background response from restoring a deleted row or
enabling actions against an older observation.

## Status collection

Each worktree uses one bounded Git command:

```text
git status --porcelain=v2 --branch -uall -z
```

The porcelain result supplies branch, upstream, ahead/behind, tracked changes,
and per-file untracked counts. An absent `branch.ab` header means zero ahead and
behind. Modified status includes modifications, additions, deletions,
conflicts, renames, copies, type changes, and untracked files.

A bounded worker pool prevents a large inventory from starting one process per
worktree. One failed worktree becomes an unknown row with a diagnostic instead
of aborting the complete dashboard. Activity ordering uses the newest time from
the worktree root, HEAD commit, and changed or untracked paths. It does not stat
every tracked file.

## Cached navigation and background removal

Cached rows can open a shell only after Kwt verifies that the directory still
exists. They can attach only after Kwt re-resolves the workspace and finds
exactly one live endpoint. This path never creates or repairs a session.
Creating, deleting, syncing, killing, and starting a workspace require current
inventory for the selected scope.

Confirmed worktree deletions enter one first-in, first-out queue. The TUI marks
active and queued rows as `removing…`, keeps other rows usable, and processes
one deletion at a time. The backend captures the removal authority while it
holds its configuration lock, then releases that lock before daemon, Git, tmux,
and fleet I/O. A known or indeterminate removal outcome triggers a current
refresh of the affected repository. Failure clears the row's removal state and
does not stop later queued work.

## Footer hints

The footer uses compact key/description cells and wraps to the terminal width.
The model reserves the rendered footer height so wrapped hints do not push the
status line off-screen.

## Testing expectations

TUI tests should assert user-visible behavior: rendered rows, active
perspective, selection/cancel behavior, handoff intent, and command outcomes.
They should not assert that helper functions are wired exactly as they happen to
be written.
