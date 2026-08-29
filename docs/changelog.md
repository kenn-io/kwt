---
title: Changelog
description: Release history for kwt
---

# Changelog

## 0.5.1

<small>2026-08-29</small>

kwt 0.5.1 is the publishable build of the 0.5 release. The original 0.5.0 tag
did not produce release artifacts because its verification jobs depended on
the host runner's process table and path syntax. This release makes those
checks portable without changing the CLI's removal safeguards.

Applications that embed kwt can now supply
`RemovalServiceOptions.ProcessGuard` when they already have their own way to
detect processes that use a worktree. kwt's default remains unchanged: removal
stops when a process uses the worktree or when that check is inconclusive,
unless the caller explicitly uses `Force`.

[Compare v0.5.0...v0.5.1](https://github.com/kenn-io/kwt/compare/v0.5.0...v0.5.1)

## 0.5.0

<small>2026-08-28</small>

kwt 0.5.0 makes the complete worktree lifecycle safer to manage and easier to
automate. It adds practical maintenance and change inspection, moves shared
state behind a local daemon, and exposes the same core services to applications
that embed kwt.

### Maintain worktrees safely

- Find broken worktree metadata and stale project registrations with
  `kwt doctor`. The command is read-only by default. `kwt doctor --fix` makes
  only repairs that kwt can prove are unambiguous, then scans again and leaves
  uncertain cases for you to review.
- Preview expired or merged worktrees before removing them with
  `kwt prune --expired --dry-run` or `kwt prune --merged --dry-run`. Merged
  pruning requires a confirmed GitHub pull request and preserves the local
  branch. A dirty merged worktree requires interactive confirmation, so an
  unattended cleanup fails closed instead of discarding work.
- See progress, the current phase, and an estimated completion time during
  longer doctor and prune runs. The terminal dashboard stays responsive while
  removals wait in a first-in, first-out background queue.
- Avoid deleting a checkout from under a shell, agent, or other local process.
  Removal now stops when a live process is using the worktree as its current
  directory and reports the blocking process IDs. `kwt remove --force` remains
  the explicit override.
- Treat a worktree as removed when Git has deregistered it but could not delete
  every file. kwt finishes its guarded bookkeeping and reports
  `cleanup_incomplete` with the remaining path instead of hiding the partial
  result behind a generic failure.

### Run dependable workspaces

- Register and open ordinary directories as workspaces, even when they are not
  Git repositories. Directory workspaces use the same layouts, structured
  inventory, and session-starting commands as worktree workspaces.
- Use one current view of projects and worktrees across the CLI and dashboard.
  A local kwt daemon now owns inventory and worktree removal, while the
  dashboard can paint its last known view immediately and refresh the active
  project first.
- Keep kwt sessions separate from tmux sessions you manage yourself. New
  ordinary workspaces and `kwt tmux run` jobs use the dedicated `tmux -L kwt`
  server. kwt can adopt a verified existing workspace session from the default
  server during the transition, but it does not create a new session there.
- Resolve a workspace session from the exact observed worktree generation, so
  replacing or switching a checkout at the same path cannot silently attach a
  client to the old workspace.
- Import a pull request again after its old worktree has been removed. kwt now
  reuses the preserved local branch when it creates the replacement worktree.

### Automate with confidence

- Inspect the exact changed files in one registered worktree with
  `kwt changes [path]`. Human output separates staged and working-tree changes;
  `--json` returns a stable, deterministic list including untracked files,
  renames, copies, deletions, and conflicts. kwt discards the result if the
  worktree is replaced while Git is reading it.
- Follow long-running daemon work through ordered, reconnectable operation
  events. Stable error codes distinguish ownership, compatibility, transport,
  stale-state, and unknown-outcome failures without making clients parse prose.
- Bind destructive or identity-sensitive actions to the inventory that a
  client actually reviewed. Guarded project removal, worktree creation,
  protected attachment, and pull-request import stop with a retryable error if
  the project registration, worktree generation, or session changed first.
- Remove stale project metadata noninteractively with
  `kwt projects remove <path> --json`. This command removes only the registry
  entry; it never deletes a repository, worktree, or tmux session.

### Embed and connect kwt

- Build on public Go services for project and worktree inventory, removal,
  change inspection, operations, typed errors, and SSH. These are supported kwt
  building blocks; Ghosthub is one consumer, not the only intended client.
- Use the authenticated local daemon when a separate process needs current
  inventory, lifecycle ownership, ordered progress, or stable failure details.
  Remote clients still run kwt on the remote machine and connect only to that
  machine's loopback daemon.
- Attach clients through an explicit tmux session endpoint. JSON records carry
  `session_name`, `tmux_socket_name`, and `tmux_attach_mode`, so a client does
  not have to infer whether a workspace uses the dedicated server, an adopted
  default-server session, or a protected socket. These endpoint fields are a
  client contract, not daemon tmux routes.
- Resolve OpenSSH routes and let kwt hold generation-bound connection leases.
  Clients can review structured host-key prompts, keep direct and ProxyJump
  connections alive, run commands, and copy files without reconstructing kwt's
  private SSH arguments. Unattended connections require already trusted keys;
  reviewed connections use an explicit host-key policy.

### Upgrade notes

- Bare `kwt prune` no longer repairs structural metadata. Use
  `kwt doctor --fix` for that work.
- `kwt prune --expired` now applies only to live worktrees covered by an
  expiration policy. An already-absent path reports `doctor_required` and
  points you to `kwt doctor --fix` instead of silently removing its registry
  entry.
- The general minimum remains Git 2.20. `kwt doctor`,
  `kwt prune --expired`, and `kwt prune --merged` require Git 2.31.
- Building from source, including `go install`, now requires Go 1.27. Supported
  macOS versions start at macOS 13.
- `KWT_HOME` now isolates `registry.json` as well as configuration and
  pull-request state. If you move an existing installation to a new
  `KWT_HOME`, copy the old `registry.json` explicitly; kwt will not import it
  automatically.
- Ordinary workspaces now use kwt's dedicated tmux server. Upgrade cooperating
  kwt clients, including embedded clients such as Ghosthub, together during the
  transition so an older client does not create a parallel default-server
  session.

### Fixes

- Project listings omit registered paths that no longer exist, while retaining
  their stored identity so registering the checkout at its new path can still
  relocate the project.
- Empty project registries are repaired without overwriting newer or concurrent
  registration changes.
- Pull-request re-import works after the previous worktree was removed and its
  local branch was preserved.

[Compare v0.4.0...v0.5.0](https://github.com/kenn-io/kwt/compare/v0.4.0...v0.5.0)

## 0.4.0

<small>2026-08-02</small>

kwt 0.4.0 expanded the project from creating new-branch worktrees into managing
workspaces from existing branches and pull requests. It also made worktree
identity and tmux handoff safer for both interactive use and automation.

### Start from work that already exists

- Create a worktree from an existing local or remote branch with
  `kwt add --from`. The dashboard offers the same branch search with `b`, and
  automation can read the normalized choices from `kwt branches --json`.
- Open exact primary or linked-worktree paths outside the configured global
  worktree directory. `kwt open <path> --start-session` can prepare a missing
  workspace without attaching to it.
- Import pull requests with the host's configured Git credential helpers and
  keep their workspace history when a GitHub repository is renamed or
  transferred.

### Keep workspace identity intact

- Bind destructive worktree actions to the checkout's durable creation
  identity. A replacement checkout at the same path is not removed because of
  a stale confirmation.
- Finish bookkeeping when Git deregisters a worktree but cannot complete the
  final directory removal, while continuing to preserve ordinary dirty-worktree
  errors.
- Create missing protected and ordinary tmux sessions before attachment. On
  Unix-like systems, kwt hands an external attachment directly to tmux instead
  of remaining resident for the session's lifetime; Windows keeps the child
  process behavior.
- Make inventory easier to read by labeling primary checkouts with `[primary]`
  and detached worktrees with their abbreviated commit, including in
  multi-machine views.

### Install a released build

- Download checksummed archives for macOS, Linux, and Windows on AMD64 and
  ARM64. Development installs also use the shared GOPATH binary directory by
  default, so the same `kwt` binary resolves across repositories.

[Compare v0.3.0...v0.4.0](https://github.com/kenn-io/kwt/compare/v0.3.0...v0.4.0)

## 0.3.0

<small>2026-07-27</small>

kwt 0.3.0 established the `vMAJOR.MINOR.PATCH` release line and made project
registration available to automation. `kwt projects add <path> --json` lets a
new machine register an existing checkout without opening the dashboard or
editing configuration by hand. Repeating the command converges on one canonical
project entry instead of creating duplicates.

This release remains installable through the Go module proxy. It predates the
automated GitHub archive and checksum pipeline introduced in v0.4.0.
