# kwt: Git worktree manager

kwt is a Git worktree manager for people and coding agents. It creates one
isolated checkout per branch, opens a tmux workspace in it, shows the state of
every worktree across your projects, and removes worktrees safely. People use
the dashboard. Agents and scripts use the command line, which prints JSON.

```sh
go install go.kenn.io/kwt/cmd/kwt@latest
cd your-repo
kwt          # registers the project and opens the dashboard
```

One Go binary. Prebuilt macOS, Linux, and Windows archives with checksums are
on [GitHub Releases](https://github.com/kenn-io/kwt/releases). Workspaces need
Git 2.20 and tmux 2.1 or newer. See
[all install options](https://kwt.sh/docs/get-started/install/).

## One worktree and one tmux session per branch

Each branch gets its own checkout and its own tmux session, created by one
command. Parallel work never shares a working directory. The dashboard lists
every worktree in every repository you have registered.

- `kwt add -b <branch>` creates the branch, the worktree, and the workspace.
  `--no-launch` skips tmux.
- `kwt` in any repository registers it. A directory that is not a repository
  can be registered as a tmux workspace.
- A local daemon starts on demand to own the shared inventory and exits when
  idle.
- `KWT_HOME` holds the config, the project registry, and pull-request records,
  so kwt's state can be isolated or backed up as one directory.

## Every action is a command with JSON output

Agents do not need the dashboard. Create a worktree, resolve its path, run a
command in it, list what changed, and remove it, each with a documented
result. Results carry the worktree's generation, so a snapshot of a checkout
that was replaced during inspection is rejected rather than returned.

```sh
kwt add -b fix/flaky-status --no-launch
kwt exec fix/flaky-status -- go test ./internal/status
kwt changes "$(kwt get fix/flaky-status)" --json
kwt list --json
kwt remove -b fix/flaky-status
```

- `kwt list --json` returns path, branch, commit, `generation`, repository
  slug, and the tmux session endpoint for each worktree. It reads live state
  and fails rather than print cached data.
- `kwt changes --json` separates staged from working-tree changes and includes
  untracked files, renames, deletions, and conflicts. `--expected-generation`
  ties the result to the checkout you reviewed.
- Failures write `{ "error": { "code", "retryable", "details" } }` to stdout
  and one line to stderr. Scripts should branch on the code and exit status,
  not the message text.
- `kwt open <path> --start-session --json` creates the tmux session without
  attaching and reports `session_name`, `tmux_socket_name`, and
  `tmux_attach_mode` so another terminal client can attach.

## Checkouts of existing branches start inert

When kwt checks out an existing local or remote branch, or imports a pull
request, it runs no repository hooks, setup commands, or file copies, and
starts no tmux session or agent. You review the checkout with `kwt changes`.
Only an explicit `kwt open` starts the workspace.

- `kwt pr import` creates an inert checkout that keeps the contributor
  branch's push destination. The only way to attach is `kwt pr attach`.
  `kwt open` and the dashboard refuse imported pull-request worktrees.
- `kwt remove` refuses a worktree with uncommitted changes, and refuses a
  worktree that a running process has as its working directory, listing the
  process IDs. `--force` overrides both checks.

## Dashboard, CLI, Go package, daemon, tmux, and SSH

A local daemon owns the shared inventory and serializes changes to it. The
dashboard (`kwt`, `kwt tui`), the CLI with JSON output, the `go.kenn.io/kwt`
Go package, the tmux session endpoint for terminal clients, and reviewed SSH
routes all use the same registry and the same safety rules.
[Ghosthub](https://ghosthub.ai) embeds these services. Nothing in them is
specific to Ghosthub.

## Get started

- [The guide](https://kwt.sh/guide.md): registering a project, creating
  worktrees, layouts, inspection, pull-request import, cleanup, multi-machine
  sync, and embedding kwt.
- [Documentation](https://kwt.sh/docs/): commands, JSON fields, configuration
  keys, and design notes. Every page is also served as Markdown.
- [GitHub](https://github.com/kenn-io/kwt): source, issues, releases.
  Apache-2.0.
