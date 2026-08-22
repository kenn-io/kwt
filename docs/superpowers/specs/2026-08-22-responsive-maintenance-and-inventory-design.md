# Responsive Maintenance and Inventory Design

**Issue:** `wbcw`

## Goal

Make `kwt doctor`, `kwt prune`, and the TUI explain ongoing work and remain
useful while slow work runs. Preserve the existing fail-closed removal rules,
but distinguish a confirmed merged pull request with local changes from an
unrelated worktree.

## Current problems

- `kwt doctor` and `kwt prune` can run for several seconds without saying what
  they are doing.
- `kwt prune --merged` checks cleanliness before it proves that a pull request
  merged. This reports every dirty worktree as a pruning problem, including
  worktrees that are unrelated to a merged pull request.
- The TUI paints cached rows quickly, but blocks navigation and mutations until
  a global refresh and status collection finish.
- Status collection starts one goroutine per worktree and invokes several Git
  processes per worktree. Activity calculation also stats every tracked file.
- Worktree removal holds the TUI backend lock and blocks refresh and other use
  until removal, publication, and session cleanup finish.

## Non-goals

- Do not replace command-line Git with `go-git` in this change.
- Do not add a daemon inventory protocol version or partial daemon-cache merge.
- Do not make status collection or worktree mutation fully concurrent.
- Do not weaken generation, pull-request identity, protected-session, or
  lock-scoped removal checks.
- Do not add an automatic override for a branch that advanced after its pull
  request head.

## Maintenance progress

Introduce one small progress reporter shared by `doctor` and `prune`. It writes
only to the command's error stream so human progress never changes structured
stdout.

When the error stream is a terminal, the reporter owns one updating line. It
starts with a spinner and the current phase. After a phase knows its total, it
shows `completed/total` and an estimated time remaining based on completed
items. It clears the transient line before a prompt, diagnostic, or final
report and redraws it if work continues.

When the error stream is not a terminal, the reporter emits bounded milestone
lines: phase start, periodic count milestones, and phase completion. It does
not write one line per candidate. `--json` keeps its current stdout schema and
may still receive progress on stderr. `doctor --quiet` suppresses both the
human report and progress because the user explicitly requested silence.

Expected phases are:

- doctor: load inventory, inspect worktrees, apply fixes, and verify repairs;
- expired prune: load candidates, validate candidates, and remove candidates;
- merged prune: discover candidates, verify pull requests, and remove
  candidates.

The progress interface accepts phase changes and completed counts. Business
logic does not know about spinner frames, terminal widths, or ETA formatting.
Cancellation and all exits close the reporter so a partial status line is not
left behind.

## Merged-prune classification and confirmation

Merged pruning must establish pull-request relevance before cleanliness affects
the outcome. Candidate evaluation follows this order:

1. Reject structural preconditions that make evaluation unsafe, such as a main
   worktree, missing generation, unavailable repository identity, or invalid
   provenance.
2. Resolve the source and base repositories and find the exact associated pull
   request.
3. Confirm that the pull request is merged and that the observed branch head
   still matches the pull-request head under the existing imported and ordinary
   worktree rules.
4. Ignore candidates with no matching merged pull request. They are normal
   non-candidates and do not appear as dirty-worktree diagnostics.
5. Treat a source branch that advanced after the pull-request head as a hard
   stop. The existing `head_advanced_after_pr` outcome and manual-review
   remediation remain.
6. Check cleanliness only after steps 1–5 prove that the candidate is a merged
   worktree.

A clean confirmed-merged candidate follows the existing dry-run or removal
path. A dirty confirmed-merged candidate has a distinct confirmation-required
classification:

- During an interactive, non-JSON removal, prompt once for that worktree. The
  prompt names the path and says that its pull request merged but the worktree
  has local changes. The default answer is no.
- Yes authorizes forced Git removal for only that candidate. All ownership,
  generation, pull-request evidence, protected-session, and lock-scoped
  revalidation still run immediately before mutation.
- No records a normal declined outcome and continues. A run containing only
  successful removals, unrelated candidates, and user declines exits
  successfully.
- `--dry-run` never prompts. It reports that the dirty merged candidate would
  require confirmation and continues successfully unless another hard blocker
  exists.
- `--json` and non-interactive removal never prompt or remove a dirty candidate.
  They report `confirmation_required`, continue evaluating other candidates,
  and return the existing skipped-candidate failure status.
- `--force` remains unavailable with `--merged`; the confirmation is narrow and
  evidence-bound rather than a fleet-wide bypass.

The final human report groups repeated outcomes instead of printing the same
remediation after every path. It prints the summary after progress has stopped.
The JSON report remains a complete per-path record.

## TUI inventory architecture

### Cache and refresh ownership

The daemon remains the sole owner of the persistent global dashboard cache.
The TUI launches with the existing non-current `ViewDashboard` request and
paints cached rows immediately.

After cached rows identify the launch project, the TUI sends an existing
`ViewRepository` request with that project's working directory. Repository
views are always current and are never written to the dashboard cache. The TUI
tracks freshness per repository in memory and merges the returned project rows
onto its cached dashboard rows.

The merge must not trust the view label alone. `loadRepository` falls back to a
global result when its working directory is not a Git repository, and a deleted
path can fail during canonicalization. Before marking a project current, the
TUI verifies that every returned worktree belongs to the expected repository
identity, that a live repository view contains its main worktree, and that the
result contains no unrelated repository. A missing path, empty or global
fallback, or identity mismatch fails that project refresh and preserves the
cached rows as stale.

Repository inventory requests are non-interactive. A repository-local config
that needs trust must return `InteractionRequired` to the TUI; the refresh must
not take control of the terminal. The TUI keeps the cached rows and shows a
short status such as `project config requires trust; run kwt list there once`.

Repository-view entries update existing dashboard rows by stable path and
generation identity. They refresh structural state, endpoint state, and status,
but preserve dashboard-only repository URL and catalog fields that a repository
view does not populate. New project entries can be inserted only after their
repository identity passes scope validation. Missing project entries are
removed from that project's slice after a successful scoped refresh.

When the active project is current, the TUI starts one non-blocking current
`ViewDashboard` request per TUI session. This pass refreshes the daemon's global
cache, global directory workspaces, and launch entries. It does not trigger
global Git status collection and does not gate navigation. When it completes,
the TUI merges structural catalog changes while preserving fresher scoped
status and path-anchored selection.

Directory workspaces and launch entries have global freshness. Their mutations
require a current global result. Entering an all-project view starts a global
refresh because global data is then actively displayed.

### Status collection

Collect detailed Git status only for the active repository, or for all rows
when the user explicitly displays the all-project view. Use a bounded worker
pool instead of one goroutine per worktree.

Each worktree runs:

```text
git status --porcelain=v2 --branch -uall
```

The parser reads file state, per-file untracked entries, branch name, upstream,
and ahead/behind counts. If `branch.ab` is absent, ahead and behind are both
zero. HEAD commit time is obtained with one small Git query per worktree; a
later benchmark may batch these queries, but this design does not require it.

Last activity is the maximum of:

- HEAD commit time;
- worktree root modification time; and
- modification times of changed and untracked files named by porcelain output.

The collector does not list or stat every tracked file. An ordinary status
failure returns an unknown status plus a per-row diagnostic so other rows still
render. Context cancellation or failure to obtain the scoped inventory cancels
the refresh as a whole.

Rows remain sorted by activity. After every merge or reorder, selection is
restored by the existing path anchor before falling back to the containing row.

### Cached-row actions

Navigation and search always remain available. `c` is available on a cached
worktree while its project refreshes. Immediately before shell handoff, it
checks that the selected path still exists and is a directory; a failed check
keeps the TUI open and shows a useful message.

Attaching to a cached row uses a new live-only backend method. It re-resolves
the exact workspace path, generation, and session name, requires one verified
live endpoint, and then attaches or prepares the resident attach command. It
never calls `Establish`, never creates a session, and fails closed when no exact
live endpoint exists or resolution is ambiguous.

The existing establish path remains available only when the relevant project
or global scope is current. Creation, removal, sync, session creation, session
kill, and directory-workspace mutation stay freshness-gated according to their
scope.

## Asynchronous worktree removal

The TUI owns a first-in, first-out removal queue with one active removal. A
confirmed deletion immediately marks its row `removing…`, returns control to the
event loop, and starts the worker if the queue was idle. More confirmed
deletions append to the queue. This is deliberately single-file; daemon and Git
locking continue to coordinate other kwt clients and agents.

The backend must not hold its inventory/configuration mutex for the duration of
the daemon removal, fleet publication, or tmux cleanup. It snapshots the
immutable dependencies needed for one request under the lock, releases the
lock, then performs the operation. Configuration replacement and inventory
refresh can therefore proceed while removal runs.

On success, the model removes the optimistic row, reports completion, and
starts a scoped refresh for that project. On a known failure, it clears the
`removing…` state, restores the row, and shows the typed error without stopping
the next queued removal. When the daemon reports an unobservable outcome, the
model preserves the existing fail-closed reconciliation behavior: do not claim
success, refresh the affected scope, and do not kill an endpoint unless removal
is known to have happened.

The user can navigate, search, shell into another existing path, and attach to
a verified live session while the queue runs. Mutations that target a row
already queued or removing are rejected. Other mutations continue to use the
existing daemon/project locks; this change does not promise parallel mutation.

## Error presentation

- Transient progress uses one status area, not an unbounded log.
- Repeated prune outcomes are summarized by reason and path group in human
  output.
- Project refresh errors stay attached to that project's freshness state and do
  not erase cached rows.
- Per-row status failures display unknown state and a compact warning.
- Background global-refresh failure leaves cached global rows in place and
  shows their age plus the last diagnostic.
- Removal failures are exceptional notifications associated with the affected
  row; they do not block navigation or later queued removals.

## Testing

Implementation follows test-first development. Tests exercise behavior rather
than source text.

- Progress tests use fake clocks and writers to verify phase transitions,
  terminal replacement, non-terminal milestone bounds, ETA display, cleanup,
  quiet mode, and unchanged JSON stdout.
- Merged-prune policy tests prove that unrelated dirty worktrees are ignored,
  dirty confirmed-merged worktrees require confirmation, yes removes only the
  confirmed candidate, no continues successfully, non-interactive and JSON
  modes fail closed, and an advanced head remains a hard stop.
- Repository-refresh tests use real temporary Git repositories where scope and
  porcelain behavior matter. They cover global fallback rejection, deleted
  paths, non-interactive trust requirements, dashboard-field preservation,
  project freshness, root-mtime activity, `-uall`, missing upstream, bounded
  concurrency, and per-row degradation.
- TUI model tests cover cached `c`, path preflight failure, live-only attach,
  freshness gates, path-anchored reordering, global directory-workspace scope,
  and background global refresh.
- Removal queue tests prove FIFO execution, immediate `removing…` state,
  continued navigation, success/failure reconciliation, duplicate rejection,
  and progress to the next queued operation.

Focused package tests run after each component. Final verification uses
`make fmt`, `make test`, `make build`, and the repository documentation check.

## Documentation

Update the CLI reference for maintenance progress and dirty merged
confirmation. Update TUI help or workflow documentation for scoped freshness,
cached-row actions, and the `removing…` state. Machine-readable schemas change
only if the new prune outcome needs a documented additive reason; the existing
schema version remains unless compatibility tests prove that an increment is
required.

## Delivery order

1. Maintenance progress and merged-prune classification/confirmation.
2. Repository-scoped TUI refresh, optimized status collection, and safe cached
   navigation/attach.
3. Single-file asynchronous removal queue and backend lock narrowing.

Each part must be independently testable and usable. The later parts may depend
on interfaces introduced earlier, but none requires a partially deployed
daemon protocol.
