---
title: Changelog
description: Release history for kwt
---

# Changelog

Notable user-facing changes to kwt, grouped by release.

## 0.4.0

<small>2026-08-02</small>

kwt 0.4.0 makes existing branches and pull requests easier to turn into
workspaces, strengthens worktree lifecycle safety, and makes tmux handoffs more
predictable.

### New features

- Create worktrees from existing local or remote branches with
  `kwt add --from`. The dashboard exposes the same branch search with `b`, and
  automation can consume the normalized inventory from `kwt branches --json`.
- Start a missing workspace without attaching by running
  `kwt open <path> --start-session`. Exact primary and linked-worktree paths now
  work even when they live outside the configured global worktree directory.
- Download checksummed release archives for macOS, Linux, and Windows on AMD64
  and ARM64 from GitHub Releases.

### Improvements

- Label primary checkouts with `[primary]` and detached worktrees with their
  abbreviated commit, including in multi-machine fleet views.
- Preserve pull-request workspace history across GitHub repository transfers
  and renames, and honor the host's configured Git credential helpers during
  authenticated imports.
- Hand external tmux attachment over directly to tmux on Unix-like systems, so
  the invoking kwt process no longer remains resident for the session's
  lifetime. Windows retains child-process attachment behavior.
- Install development builds into the shared GOPATH binary directory by
  default so they resolve consistently across repositories.

### Bug fixes

- Bind destructive worktree operations to a durable creation identity, so a
  concurrently replaced checkout cannot be removed using stale confirmation.
- Complete cleanup when Git deregisters a worktree but reports a final
  directory-removal failure, while preserving ordinary dirty-worktree errors.
- Create missing protected and ordinary tmux sessions before attachment instead
  of depending on server-specific error text or a prior interactive open.

[Compare v0.3.0...v0.4.0](https://github.com/kenn-io/kwt/compare/v0.3.0...v0.4.0)

## 0.3.0

<small>2026-07-27</small>

The first versioned kwt release established the `vMAJOR.MINOR.PATCH` release
line. It remains installable through the Go module proxy, but predates the
automated GitHub archive and checksum pipeline.
