# Short tmux Session Names

## Goal

Make KWT worktree sessions readable in terminal tabs without adding mutable
collision-allocation state.

## Contract

Standard git worktrees use this deterministic tmux session name:

```text
kwt-{repository}-{branch}-{path-hash8}
```

For example, a `feature/foo` worktree in the `kwt` repository becomes
`kwt-kwt-feature-foo-9cc4e551`. The repository and branch are sanitized with
the existing tmux-safe replacement rules. The eight-character hash continues
to use the absolute worktree path, keeping detached worktrees and otherwise
identical readable names distinct.

KWT removes the host, owner, and ceremonial `workspace` segment. Directory
workspace names are unchanged. Session parsing recognizes the new standard
worktree shape. There is no registry, conditional suffix allocator, rename,
adoption, or legacy compatibility path; the change rolls forward.

## Verification

Behavioral tests pin the short readable form, tmux-safe sanitization,
determinism, detached-worktree uniqueness, and parsing of the new form. The
full test and build targets must pass.
