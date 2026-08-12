# Short tmux Session Names

## Goal

Make KWT worktree sessions readable in terminal tabs without adding mutable
collision-allocation state.

## Contract

Standard git worktrees use this deterministic tmux session name:

```text
kwt-{repository}-{branch}-{path-hash8}
```

For example, a `feature/foo` branch worktree in the `kwt` repository becomes
`kwt-kwt-feature-foo-9cc4e551`. The repository and branch are sanitized with
the existing tmux-safe replacement rules. The eight-character hash continues
to use the absolute worktree path, keeping detached worktrees and otherwise
identical readable names distinct.

KWT removes the host, owner, and ceremonial `workspace` segment. Directory
workspace names are unchanged. The readable worktree label remains the git
branch, not the worktree directory basename.

Session parsing checks the existing 14-digit `kwt tmux run` form first, the
unchanged `kwt-workspace-dir-{name}-{hash8}` form second, and the new standard
worktree form last. This prevents the broad new worktree pattern from
misclassifying either existing family. A repository literally named
`workspace` with a branch beginning `dir-` occupies the directory-workspace
namespace; KWT accepts this narrow reserved-name ambiguity because both forms
remain workspace context and the path hash still prevents ordinary collisions.

There is no registry, conditional suffix allocator, or automatic rename. Old
ordinary worktree sessions are not adopted and the change rolls forward.

## Protected pull-request provenance

Protected pull-request sessions are the sole compatibility exception. Existing
imports keep their exact persisted session name and derived protected socket
when the record still matches the verified repository identity or alias,
branch, canonical path, and worktree generation, and the persisted name equals
either the previous or current deterministic formula for those inputs. KWT
publishes that persisted name and socket in inventory and uses them for attach,
prune, and project-removal safety checks. New imports use only the short
formula.

This bounded exception preserves the credential-isolated tmux endpoint without
allowing an arbitrary persisted session name to become trusted. It does not
provide ordinary-session adoption, fallback, or a dual attach path. Because
the new broad parser also accepts the old hyphenated shape, old sessions may
remain visible in `kwt tmux list`; worktree lifecycle operations still derive
and use only the new name.

## Verification

Behavioral tests pin the short readable form, tmux-safe sanitization,
determinism, detached-worktree uniqueness, and parsing precedence across all
three managed name families. Pull-request tests pin both accepted deterministic
formulas, reject arbitrary persisted names, and verify that protected inventory
continues to publish the persisted endpoint. Pull-request documentation
examples use the new short form. The full test and build targets must pass.
