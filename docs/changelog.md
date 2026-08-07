---
title: Changelog
description: Release history for kwt
---

# Changelog

Notable user-facing changes to kwt, grouped by release.

## Unreleased

### New features

- Diagnose moved or deleted project registrations, broken linked-worktree
  backlinks, stale Git records, and registry drift with read-only `kwt doctor`.
  `kwt doctor --fix` relocates one unambiguous identity match, removes an
  unchanged stale registration when no match exists, removes stale duplicates
  when the matching live repository is already registered, and leaves multiple
  clones or multiple registrations claiming one target for manual choice.
- Read large doctor reports as an action-first dashboard: safe repairs appear
  before findings that need review, healthy repositories and opaque generation
  values are omitted, and terminal output adapts to width and color settings.
  After `doctor --fix`, completed repairs are listed before remaining findings;
  `--quiet` suppresses the report when only the exit status matters. Versioned
  JSON includes both the completed repairs and the final rescan for automation.
- Adopt generation-less live registry records with guarded compare-and-swap
  after clearing inherited expiration metadata; nonempty generation conflicts
  remain manual. Registry reconciliation uses canonical path identity so
  symlink aliases still match live worktrees. Repository-wide Git metadata
  pruning is also declined whenever any removable record is outside the
  verified fix scope.
- Diagnose multiple registry path spellings that resolve to one live worktree.
  `kwt doctor --fix` collapses a complete group only when all policy metadata
  agrees. Equivalent aliases for a confirmed-absent path are removed as one
  guarded group in the same fix run, including when a symlink appears above
  multiple missing path components; conflicting or incompletely inspected
  aliases remain visible for manual review.
- Report existing registry paths that are not verified Git worktrees as
  `unverified_registry_entry`; these records remain manual even when they carry
  a valid-looking generation.
- Preview or remove clean linked worktrees for explicitly merged GitHub pull
  requests with `kwt prune --merged [--dry-run]`. Exact head-SHA evidence works
  for squash and rebase merges while preserving local branches.

### Breaking changes

- Keep the general minimum supported Git version at 2.20. `kwt doctor` and the
  `kwt prune --expired` or `--merged` policies require Git 2.31 because their
  inventory relies on worktree annotations introduced in 2.31; structural
  repair also uses Git's native worktree repair command.
- Bare `kwt prune` no longer performs Git metadata pruning. For structural
  cleanup, run `kwt doctor --fix`. `kwt prune --expired` is now only a live
  expiration policy; already-absent paths report `doctor_required` instead of
  being silently unregistered.

### Safety

- Doctor and merged pruning now fail closed when global worktree discovery
  encounters traversal, metadata access, or malformed linked `.git` file
  errors, and linked-only repositories participate in consistency inspection.
  Missing-project relocation and removal also remain manual whenever any
  configured, global, or registered repository could not be inspected, so a
  partial inventory cannot hide another matching clone. Dangling symlinks are
  reported as unreachable instead of confirmed absent.
- Doctor declines repository-wide backlink repair when any backlink Git could
  change is ambiguous or otherwise outside the validated fix scope, while
  continuing independent metadata and registry cleanup. Registry records are
  excluded only while their creator holds the path lock; abandoned tokens can
  be diagnosed and repaired under that same lock. New expiration records use
  the destination worktree's origin and persist only credential-free canonical
  repository identities; relative filesystem remotes are rejected. Doctor
  normalizes or redacts legacy registry and configured-project values before
  rendering them. Origin-based expiration records remain valid when a fork
  clone is configured under its upstream project.
- Policy removals require a valid durable generation and recheck generation,
  HEAD, repository identity, local branch, exact upstream, verified worktree
  backlink, and required cleanliness under the repository mutation lock.
  Merged pruning refuses inconsistent backlinks before running candidate-local
  Git commands. It reads worktree-specific upstream configuration, honors
  Git's `url.*.insteadOf` rewriting, and resolves GitHub base and source
  repository transfers before provider matching. Each linked worktree's
  observed origin is kept separate from the configured PR base and revalidated
  at removal. Globally discovered repositories without a configured project
  use each candidate's own origin as its PR base instead of sharing a sibling's
  fallback. Imported pull-request provenance must also match the canonical live
  workspace and main-repository paths, with symlink aliases treated as the same
  filesystem identity. Doctor and dry-run inventory remain read-only.
  Expiration pruning resolves a unique administrative directory before policy
  checks and revalidates that backlink under the same lock. It also compares the
  complete registry policy under the Git mutation lock, preserving a worktree
  when its expiration changes after inspection. New imported-PR provenance
  records carry the durable worktree generation. Ordinary open protection
  re-reads that generation from the live path whenever pull-request provenance
  exists and fails closed when it cannot verify the current worktree, so stale
  discovery and TUI rows cannot bypass protected attach. Attach and merged
  pruning ignore a nonempty recorded generation when it differs from the live
  worktree, so stale provenance cannot affect a same-path replacement;
  generation-less legacy records remain compatible.
- Global configuration writers now share one lock and atomic replacement path.
  Project repair compares the exact persisted entry, revalidates the old and
  target paths, and reloads both config and registry state before reporting the
  result, so concurrent edits remain safe no-ops. Atomic updates preserve an
  existing config-file symlink, including when its final target does not yet
  exist, its target permissions, and unknown project fields written by newer
  versions. Project relocation also refuses a target already claimed by another
  current registration inside that transaction.
- `KWT_HOME` now isolates registry state as well as configuration. kwt never
  imports the platform-config registry automatically; users moving an existing
  installation must copy `registry.json` explicitly before using the new home.
  Ordinary project registration also preserves unknown fields from newer kwt
  versions.
- Merged cleanliness checks include ignored untracked files, and every global
  candidate is removed through the stable resolved main repository rather than
  a linked worktree that an earlier removal may delete. Expiration cleanliness
  retains Git's normal treatment of ignored build artifacts. Lock-scoped
  dry-run validation reports locked and main worktrees as ineligible. Git
  commands run against merged-prune candidates with kwt's GitHub and fleet
  credentials removed, including during lock-time validation and removal.
- Merged pruning reports stale individual paths as `doctor_required` and
  continues evaluating other repositories; incomplete directory traversal
  remains fatal. When Git deregisters a worktree but leaves residual files,
  expiration and merged pruning complete guarded bookkeeping and report
  `cleanup_incomplete` with the path to inspect.
- Generation-less registry adoption rechecks the live generation under the Git
  mutation lock, and merged pruning rejects multiple canonical registry aliases,
  so same-path replacements and duplicate legacy records remain preserved for
  review.
- Worktree generation lookup now validates each administrative directory's
  reverse `gitdir` mapping and scans every claimant before accepting a match, so
  a stale backlink cannot borrow a sibling's identity and duplicate backlinks
  are reported as ambiguous. Main worktrees initialized with Git's standard
  separate Git directory layout are accepted only after the same duplicate-
  claimant scan. Doctor and merged pruning resolve those repositories from a
  verified checkout or configured `core.worktree` instead of assuming the
  common directory is `<checkout>/.git`; linked-only layouts without a reverse
  main-path fact remain manual. Merged pruning includes finalized registry-only
  worktrees, while doctor derives repository identity from the resolved main
  checkout rather than a linked worktree's origin override.
- Merged pruning retains every configured, global, and registry inventory
  root's direct `.git` claim before candidate deduplication. A copied directory
  that claims a real worktree's administrative directory now preserves the
  affected candidate as `doctor_required` while unrelated candidates continue.
- Merged pruning no longer substitutes a linked worktree's origin for a missing
  or invalid configured project identity; those candidates require doctor
  review, while live-origin fallback remains available to unconfigured roots.
- Canonically duplicate project paths retain every configured repository claim.
  Equivalent URL and slug identities remain eligible, while conflicting or
  invalid competing claims for one verified repository classify every affected
  candidate as `doctor_required` before pull-request lookup.
- Pull-request provider failures now use only declared prune reasons:
  authentication and network failures remain specific, while other provider
  errors use `provider_failure`. The detailed provider code remains available
  as evidence and every provider failure includes remediation.
- Live-origin fallback now also requires a complete configured-project
  inventory. An unavailable configured root preserves global and registry-only
  candidates as `doctor_required`, preventing a rediscovered fork from becoming
  the pull-request base; healthy configured projects remain eligible.

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
