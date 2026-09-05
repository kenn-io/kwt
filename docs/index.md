---
title: kwt documentation
description: Technical documentation for kwt, a Git worktree manager with a terminal dashboard for people and a scriptable CLI for agents and other tools.
---

# kwt documentation

kwt is a Git worktree manager for people and coding agents. It creates one
isolated checkout per branch, opens a tmux workspace in it, shows the state of
every worktree across registered projects, and removes worktrees safely.

This site documents the commands, JSON contracts, configuration, and design of
kwt. The [product overview](https://kwt.sh/) and the
[guide](https://kwt.sh/guide/) are the shorter introductions.

## Requirements

- macOS 13 or newer, Linux, or Windows.
- Git 2.20 or newer. `kwt doctor` and `kwt prune` require Git 2.31 or newer;
  `kwt pr import` requires Git 2.42.0 or newer on macOS and Linux, or Git for
  Windows 2.53.0.windows.3 or newer.
- tmux 2.1 or newer for workspace launch and `kwt tmux`. The worktree commands
  work without tmux.
- Go 1.27 or newer for `go install` and source builds.

See [Install](get-started/install.md) for every install path and
[Releases](releases.md) for versioning.

## Where to start

| Goal                                             | Page                                                      |
| ------------------------------------------------ | --------------------------------------------------------- |
| Create and open a first worktree interactively   | [Quickstart](get-started/quickstart.md)                   |
| Drive worktrees from an agent or script          | [Agent workspaces](workflows/agent-workspaces.md)         |
| Inspect, diagnose, prune, and remove worktrees   | [Worktree lifecycle](workflows/worktree-maintenance.md)   |
| Manage tmux workspaces for non-Git directories   | [Directory workspaces](workflows/directory-workspaces.md) |
| Import a pull request into an isolated checkout  | [Pull-request automation](reference/pull-requests.md)     |
| Compare worktree state across machines           | [Multi-machine sync](multi-machine-sync.md)               |
| Embed kwt in a Go application or terminal client | [Embed and connect kwt](integrations/embedding.md)        |

## Contracts

| Contract                                          | Page                                        |
| ------------------------------------------------- | ------------------------------------------- |
| Commands, JSON fields, exit status, guarded flags | [CLI reference](reference/cli.md)           |
| `config.toml`, `.kwt.toml` trust, layouts, agents | [Configuration](reference/configuration.md) |
| Trusted local state versus untrusted branch data  | [Threat model](development/threat-model.md) |
| Architecture behind each boundary                 | [Design notes](design/index.md)             |
| Release history                                   | [Changelog](changelog.md)                   |

## State and configuration

Global configuration lives at `~/.config/kwt/config.toml`, or
`$KWT_HOME/config.toml` when `KWT_HOME` is set. `KWT_HOME` also holds
`registry.json` and `pull-requests.json`, so it isolates kwt's persistent state
as a unit. Repository-local `.kwt.toml` settings are trust-gated before use.

## Safety boundaries

Worktrees created from existing local or remote branches start inert: kwt does
not run repository setup, copy configured files, or launch a workspace until
you review the checkout and open it explicitly. Pull-request imports use a
protected session boundary and preserve exact push routing. Removal refuses
dirty worktrees and worktrees in use by a live process unless `--force` is
given. Multi-machine sync is opt-in and reports advisory state only.

## For agents

Every page on this site is also served as Markdown at the same path with a
`.md` suffix, for example `https://kwt.sh/docs/reference/cli.md`, and each rendered
page advertises it with a `rel="alternate"` link. [`/llms.txt`](https://kwt.sh/llms.txt)
indexes the product, guide, and documentation pages. Machine-readable command
output is documented in the [CLI reference](reference/cli.md).
