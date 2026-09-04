# kwt: give every task its own checkout

kwt is a Git worktree manager with a terminal dashboard for people and a
scriptable CLI for coding agents and other tools. It creates isolated
checkouts, opens their tmux workspaces, shows their current state, and cleans
them up safely, across all your projects.

```sh
go install go.kenn.io/kwt/cmd/kwt@latest
cd your-repo
kwt          # registers the project and opens the dashboard
```

One Go binary. Prebuilt macOS, Linux, and Windows archives with checksums ship
on [GitHub Releases](https://github.com/kenn-io/kwt/releases). Workspaces need
Git 2.20 and tmux 2.1 or newer. See
[all install options](https://kwt.sh/docs/get-started/install/).

## 01 / Isolation: a checkout per task, a session per checkout

Two agents on one checkout is a merge conflict with extra steps. kwt gives each
branch its own worktree and its own tmux session, created together by one
command, so parallel work never shares a working directory. The dashboard shows
all of them, across every registered repository, in one place.

- **Command: 1.** `kwt add -b feature/new-ui` creates the branch, the checkout,
  and the workspace. `--no-launch` skips tmux.
- **Shared checkouts: 0.** Each task gets its own directory and session.
- **Projects: all.** Run `kwt` in any repository to register it.
- **Binary: 1.** A local daemon auto-starts to own shared inventory.
- **State: `$KWT_HOME`.** Config, the project registry, and pull-request
  records live together and isolate or back up as a unit.

## 02 / Automation: the same lifecycle as plain commands and stable JSON

```sh
kwt add -b fix/flaky-status --no-launch
kwt exec fix/flaky-status -- go test ./internal/status
kwt changes "$(kwt get fix/flaky-status)" --json
kwt list --json
kwt remove -b fix/flaky-status
```

- **Inventory.** `kwt list --json` returns path, branch, commit, `generation`,
  repository slug, and the session endpoint for each worktree. It never prints
  cached data.
- **Changes.** `kwt changes --json` separates staged from working-tree changes
  and includes untracked files, renames, deletions, and conflicts.
  `--expected-generation` binds it to the checkout you reviewed.
- **Errors.** Failures write `{ "error": { "code", "retryable", "details" } }`
  to stdout. Codes and exit statuses are the contract; messages are prose.
- **Sessions.** `kwt open <path> --start-session --json` establishes a
  workspace for another terminal client and reports `session_name`,
  `tmux_socket_name`, and `tmux_attach_mode`.

## 03 / Safety: inert until you look

When kwt checks out an existing or remote branch, or imports a pull request, it
does not run repository hooks, setup commands, or file copies, and it does not
start tmux or any agent. You review the checkout first; only an explicit open
starts the workspace.

- **Pull requests.** `kwt pr import` preserves the contributor branch's exact
  push destination. Attachment goes only through `kwt pr attach`.
- **Removal.** `kwt remove` refuses a dirty worktree or one a live process is
  inside, and lists the process IDs. `--force` is a separate decision.

## 04 / Interfaces: one lifecycle across every surface

Dashboard (`kwt`, `kwt tui`), CLI with JSON, the `go.kenn.io/kwt` Go package,
a local daemon that owns shared inventory, a tmux session endpoint for terminal
clients, and reviewed SSH routes. [Ghosthub](https://ghosthub.ai) embeds these
services; nothing in them is Ghosthub-specific.

## 05 / Start

- [The guide](https://kwt.sh/guide.md): nine stops from register to embed,
  including layouts, maintenance, pull-request import, and multi-machine sync.
- [Documentation](https://kwt.sh/docs/): commands, JSON, configuration, design.
- [GitHub](https://github.com/kenn-io/kwt): source, issues, releases.
  Apache-2.0.
