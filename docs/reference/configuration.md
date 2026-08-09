# Configuration

Global config lives at `~/.config/kwt/config.toml`, or at
`$KWT_HOME/config.toml` when `KWT_HOME` is set. Repository-local overrides live
in `.kwt.toml` and are trust-gated before use. When `KWT_HOME` is set, that same
directory also holds `registry.json` and `pull-requests.json`, isolating kwt's
persistent state as a unit. Without it, each store follows its documented
platform config-directory behavior.

`KWT_HOME` is an isolation boundary: kwt never imports registry entries from
the platform config directory automatically. To move an existing installation,
stop other kwt processes and copy its `registry.json` into `KWT_HOME` before
running kwt with the new home. Do not combine two registry files.

The global file is the source of truth for worktree naming, tmux layouts, agent
commands, repository setup rules, and the known project registry.

```toml
[worktree]
basedir = "~/.kwt/worktrees"
auto_mkdir = true

[naming]
template = "{{.FullPath}}/{{.Branch}}"

[naming.sanitize_chars]
"/" = "-"
":" = "-"

[agents]
codex = "codex"
claude = "claude"
roborev = "roborev tui"

[layouts]
# default = "quad"  # unset or "none" = blank single-pane session
auto_launch_on_add = true

[daemon]
idle_timeout = "2h"        # 0s disables automatic background shutdown
auto_restart = "newer"     # newer or never
replacement_grace = "5m"  # must be positive

[[layouts.presets]]
name = "quad"
arrange = "even-horizontal"
panes = ["agent:codex", "agent:claude", "agent:roborev", ""]

[[layouts.presets]]
name = "stack"
arrange = "even-vertical"
panes = ["agent:codex", "agent:claude", "agent:roborev", ""]
```

The default places newly created worktrees under `~/.kwt/worktrees`. Relative
paths in a trusted repository-local `.kwt.toml` are resolved from that
repository's root; a repository-local `worktree.basedir` cannot be empty.
Repository-local path fields cannot reference environment variables, including
`naming.template` and `naming.sanitize_chars` replacements. Generated paths
influenced by repository-local naming are not environment-expanded after
rendering, so a template cannot synthesize a reference. Environment expansion
remains available for paths in the global configuration and for explicit
command-line paths. In a global naming template, expansion applies only to
literal template text, preserving Go template variables inside actions.
Global `naming.sanitize_chars` replacement values expand before branch
sanitization.

Daemon lifecycle policy is global-only. Repository-local `.kwt.toml` files
never control daemon lifetime, replacement, or authority. `idle_timeout`
applies only to the detached background daemon; `kwt serve` stays active until
it is stopped. With `auto_restart = "newer"`, a newer compatible kwt binary
drains and replaces an older daemon, while an older binary continues using a
newer daemon. `auto_restart = "never"` disables automatic version replacement.

Resolved worktree and directory-workspace paths cannot contain `#`, which tmux
reserves for format expansion. kwt rejects such paths before creating or
registering a workspace.

Pane entries are shell commands. `agent:<name>` expands through the `[agents]`
table before tmux starts, so command flags live in one local config file.

`layouts.default` is optional. When it is unset — or set to the reserved name
`none` — workspaces launch as a blank single-pane session in the worktree
directory. Repository-local `.kwt.toml` files may also set
`layouts.default = "none"` to opt a single project back into blank sessions
when the global config names a preset.

## Project registry

The dashboard lists worktrees from the configured base directory, the current
launch repository, and registered projects. Running `kwt` inside a repository
registers or refreshes that repository so future dashboard launches can find its
worktrees from anywhere.
Automation and graphical clients can perform the same explicit registration
without opening the dashboard by running `kwt projects add <path> --json`.
They can unregister that metadata with `kwt projects remove <path> --json`;
the command never deletes repository, worktree, or tmux-session data.

Project entries are discovery metadata, not worktree-creation policy:

```toml
[[projects]]
repository = "github.com/kenn-io/kwt"
name = "kwt"
path = "~/code/kwt"
last_touched = "2026-07-04T12:00:00Z"
```

Project refreshes update these known fields without discarding additional
fields written by a newer kwt version.

## Directory workspaces

Plain directories registered as tmux workspaces, independent of any Git
worktree:

```toml
[[workspaces]]
name = "notes"
path = "~/notes"
```

Paths are expanded and symlink-resolved on load. Entries are machine-level
configuration: repository-local `.kwt.toml` files cannot set them, and they are
never published over multi-machine sync. Manage entries with
`kwt workspace add|list|remove` rather than editing the file; see
[Directory workspaces](../workflows/directory-workspaces.md) for the workflow.

## Repository setup

Optional repository settings can copy files or run commands when new worktrees
are created by `kwt add`:

```toml
[[repository_settings]]
repository = "~/code/myapp"
basedir = "./worktrees"
copy_files = ["templates/.env.example"]
setup_commands = [
  "npm install",
  'printf "branch=%s\npath=%s\n" "{{.Branch}}" "{{.Path}}" > .worktree-info',
]
```

Template variables include `Host`, `Owner`, `Repository`, `FullPath`, `Branch`,
`Hash`, and `Path`. Quote variables in shell commands when values may contain
spaces. `repository` may also be a glob such as `**/acme/widget`; trusted
repository-local glob selectors remain repository selectors rather than being
resolved as paths beneath that repository.

Remote-only multi-machine sync skips repository setup (`copy_files` and
`setup_commands`) because the branch name is reported by another host. Run any
project bootstrap command manually after syncing if that branch needs it.

## Multi-machine sync config

Multi-machine sync is an opt-in subsystem. The public command namespace is
`kwt sync`, and the config section is `[fleet]`. All `[fleet]` settings must be
set in the global `config.toml`; values in repository-local `.kwt.toml` files
are ignored.

```toml
[fleet]
enabled = true
host_id = "host-a"
hub_url = "https://host-a.example"
token_file = "~/.config/kwt/fleet.token"

[fleet.hub]
listen_addr = "127.0.0.1:8787"
store_path = "~/.local/share/kwt/fleet/state.json"
```

Plain `http://` hub URLs and hub listeners are accepted only for loopback
hosts. Loopback client requests bypass environment HTTP proxies. Every
multi-machine hub URL must use HTTPS, commonly by serving the loopback hub
through a private TLS endpoint.

See [Multi-machine sync](../multi-machine-sync.md) for the user-facing workflow
and [Multi-machine sync architecture](../design/multi-machine-sync.md) for the
wire protocol and hub behavior.
