# Worktree Labels Design

## Problem

The TUI dashboard currently renders the primary checkout's branch like any
linked worktree and renders a detached worktree as the literal branch `HEAD`.
As a result, users cannot tell why deleting the primary checkout is refused or
what the `HEAD` row represents.

For example, when launched from `~/code/middleman`, the primary checkout
appears as `create-session-no-focus`, while a temporary detached worktree
appears as `HEAD`. Attempting to delete the former reports only
`refusing to remove a main worktree`.

## User-facing behavior

The branch column and every other user-facing row label will distinguish these
states:

- A primary checkout will append `[primary]` to its branch, for example
  `create-session-no-focus [primary]`.
- A detached worktree will use `detached@` followed by the first eight
  characters of its commit hash, for example `detached@be094b1b`.
- An ordinary linked worktree will continue to display its branch unchanged.

The same presentation applies to remote-only fleet rows. A remote detached row
will derive its short hash from the fleet ref. A remote-only branch row will
append `[primary]` when every observation represented by that row identifies
the checkout as primary. A row with mixed primary and linked observations will
remain unmarked because primary status is host-specific and the grouped row
cannot truthfully claim one checkout type.

Attempting to delete a primary checkout will report its role and location:

```text
cannot delete the primary checkout: ~/code/middleman
```

The primary checkout remains protected. This change does not add a workflow
for deleting an entire repository.

## Implementation boundaries

Raw branch data remains unchanged. Repository matching, fleet keys, sorting
identity, Git operations, and backend APIs will continue to use the actual
branch value (`HEAD` for a detached checkout).

A TUI presentation helper will derive the display branch from local entry
fields when an entry exists, or from the fleet kind, ref, branch, and summarized
primary state for a remote-only row. The TUI fleet adapter will retain whether
all observations for a remote-only row are primary; it will not collapse mixed
observations into a primary marker.

Dashboard cells and every `rowLabel` consumer—including selection details,
confirmation prompts, creation messages, sync messages, and removal
messages—will use the display value. Filtering will include the display value
so users can search for `primary`, `detached`, or a displayed short hash without
changing the underlying row identity.

The protected-delete message will abbreviate the user's home directory in the
same way as the existing selection details.

## Error handling

If a detached local row has no commit hash, or a detached remote row has no
ref, its display label will be `detached` rather than exposing the ambiguous
literal `HEAD`. Short commit hashes will be used as-is.

The backend's independent primary-checkout guard remains in place as a
defense-in-depth check. Only the TUI's immediate feedback becomes more
specific.

## Testing

Focused TUI tests will cover:

- primary checkout display labels;
- detached local display labels with full, short, and missing commit hashes;
- remote-only detached display labels derived from fleet refs;
- uniformly primary and mixed-primary remote-only fleet rows;
- unchanged labels for ordinary linked worktrees;
- filtering by the new display labels;
- the protected-delete message including the abbreviated checkout path.

The repository test and build commands will verify that the presentation
change does not alter worktree identity or other CLI behavior.
