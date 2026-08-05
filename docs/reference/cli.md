# CLI Reference

Run `kwt <command> --help` for command-specific flags. This page summarizes the
stable command surface.

| Command          | Purpose                                                |
| ---------------- | ------------------------------------------------------ |
| `kwt`, `kwt tui` | Open the cross-project and multi-machine dashboard.    |
| `kwt add`        | Create a worktree and optionally launch its workspace. |
| `kwt branches`   | List branches available for a new worktree.            |
| `kwt open`       | Open or establish a worktree workspace session.        |
| `kwt list`       | List worktrees.                                        |
| `kwt status`     | Show Git status, sync state, and activity.             |
| `kwt projects`   | List registered project repositories.                  |
| `kwt pr`         | Discover and import pull requests through JSON.        |
| `kwt get`        | Print a matching worktree path.                        |
| `kwt cd`         | Open a shell in a matching worktree.                   |
| `kwt exec`       | Run a command in a matching worktree.                  |
| `kwt remove`     | Delete a worktree, optionally its branch.              |
| `kwt prune`      | Clean up stale Git worktree metadata.                  |
| `kwt sync`       | Publish and inspect multi-machine sync state.          |
| `kwt tmux`       | Manage standalone tmux sessions.                       |
| `kwt workspace`  | Manage directory workspaces.                           |
| `kwt config`     | Read and write config values.                          |
| `kwt completion` | Generate shell completion and integration.             |
| `kwt version`    | Show version and build information.                    |

## Examples

```sh
kwt add -b fix/parser-race
kwt add --from origin/fix/review fix/review
kwt branches --json
kwt open parser
kwt open /path/to/worktree --start-session
kwt status
kwt pr list --project github.com/acme/widget --json
kwt pr import 17 --project github.com/acme/widget \
  --start-session --json
kwt sync status
kwt exec fix/parser-race -- go test ./internal/parser
kwt workspace add ~/notes
kwt workspace list
kwt config get layouts.default
kwt config set --local layouts.default stack
```

When `kwt add -b` creates a branch, it fetches `origin` and starts from its
default branch. If that remote base is unavailable, it falls back to local
`main`, then `master`, then the branch checked out in the primary worktree.

`kwt add <branch>` checks out an existing local branch. Use
`kwt add --from <remote-ref> <branch>` when a remote candidate must become a
local tracking branch. Shorthand such as `origin/topic` is accepted, but kwt
verifies it against fetched refs and passes the full `refs/remotes/...` identity
to Git. Neither existing-branch path copies files, runs setup commands, or
launches a workspace; `--layout` and `--select-layout` are rejected.
Git branch mutation and checkout run with an empty hooks directory, configured
smudge and process filters disabled, and kwt credential variables removed.
Environment references in the source-derived branch name remain literal when
kwt builds the destination path. Submodules are not recursively updated during
creation; inspect the superproject first, then update submodules explicitly
after acknowledgement.

The created worktree participates in ordinary status and fleet observation.
Review the checkout and run `kwt open <worktree>` as the explicit
acknowledgement that opts in to its layout and pane commands.

`kwt branches --json` emits only candidates not already checked out, with
`name`, a source-qualified display `label`, the full source ref, and
`is_remote` fields for interactive clients.

## `kwt open`

With no argument, `kwt open` fuzzy-picks a worktree. A pattern narrows the
cross-project list and opens the sole match directly. Kwt creates or repairs
the canonical tmux workspace with its resolved layout before attaching.

An exact worktree-root path is resolved directly from Git before pattern
matching, including registered primary checkouts and linked worktrees outside
the configured global worktree base.

On Unix-like systems, an external ordinary or protected attachment replaces
the `kwt` process with the tmux client. tmux therefore owns signal handling and
the final exit status, and no waiting `kwt` parent remains. Windows retains a
waiting parent because it has no Unix process-replacement primitive.
An ordinary open from inside tmux switches the current client instead.
Protected attachment always remains external because it targets a separate
workspace-specific socket.

`kwt open <exact-worktree-path> --start-session` performs the same layout and
session bootstrap without attaching a client. Use it before an external
ordinary tmux client attaches to a session that may not exist yet. The exact
path is resolved directly from Git rather than the global worktree base, which
keeps this automation mode noninteractive and supports linked worktrees stored
outside that base.
Protected pull-request imports remain restricted to `kwt pr attach`.

## `kwt list`

`--json` emits an array of objects with `path`, `branch`, `commit_hash`, `is_main`,
`created_at` (worktree directory mtime), `generation` (the durable identity for
conditional removal), `repository` (the `host/owner/name` slug, or a
`local/<path>` fallback for a repository without a usable remote — see below),
and `session_name` (the tmux workspace session name kwt attaches to).
An imported pull-request worktree additionally includes `tmux_socket_name` for
its protected workspace-specific server. To converge on the same session, run
`kwt open <path> --start-session` before an ordinary attach-only client, or
`kwt pr attach <path>` when `tmux_socket_name` is present. This lets kwt create
the session when needed without a client creating it bare or bypassing its
protected attach policy. See [Attaching from other
tools](#attaching-from-other-tools) before using `new-session`.
`kwt open` and dashboard open actions refuse protected pull-request imports
and direct the user through `kwt pr attach`.
`created_at` and `generation` are populated in both local and `-g` mode.

## `kwt remove`

`--if-generation <id>` makes a single-worktree removal conditional on the
`generation` value returned by `kwt list --json`. Kwt stores this random
identity in the worktree's Git administrative directory and compares it while
holding the repository's worktree-mutation lock, so automation cannot delete a
replacement checkout created at the same path. Ordinary directory changes do
not alter the generation.

## `kwt projects`

`--json` emits an array of the registered project repositories (`{repository,
name, path, last_touched}`), so external automation can discover main-repo
paths that live outside the configured worktree base directory without
parsing the config file. `repository` uses the same `host/owner/name` slug as
`kwt list --json`'s `repository` field, so the two surfaces can be joined.

Entries whose configured paths are missing, inaccessible, not Git repositories,
or empty are omitted from both table and JSON output. Kwt does not delete those
registry records or scan for moved checkouts; running kwt in the checkout's new
location, or using `kwt projects add <new-path>`, updates the existing record by
repository identity.

`kwt projects add <path>` registers an existing Git checkout without opening
the dashboard. A linked-worktree path resolves to its main repository before
registration, and repeating the command updates the existing entry rather than
adding a duplicate.

With `--json`, success returns:

```json
{
  "status": "registered",
  "project": {
    "repository": "github.com/kenn-io/kwt",
    "name": "kwt",
    "path": "/code/kwt",
    "last_touched": "2026-07-27T11:16:16Z"
  }
}
```

Failures return a stable error envelope on stdout. `invalid_repository` exits
2; `registration_failed` exits 1. Both are non-retryable:

```json
{
  "error": {
    "code": "invalid_repository",
    "message": "/missing is not an accessible Git repository",
    "retryable": false
  }
}
```

## `kwt pr`

`kwt pr list` and `kwt pr import` are the noninteractive, structured boundary
for pull-request clients. kwt owns provider calls, ref handling, branch and
workspace naming, normal worktree creation and setup, push configuration,
provenance, and tmux session naming. See [Pull-request
automation](pull-requests.md) for the JSON and exit-status contract.
Every imported workspace record includes `tmux_socket_name`.
`pr import --start-session` additionally establishes a blank shell-only
session without attaching, for clients that provide their own ordinary tmux
presentation. It does not execute configured layouts or agent commands.
Attach with `kwt pr attach <workspace.path>`, which verifies the persisted
identity, creates or repairs that protected blank session when needed, and
uses `attach-session -E`. This is an interactive exception to the PR JSON
contract: failures before attachment remain structured, but after a successful
Unix process replacement tmux owns terminal output and the final exit status.

### Repository identity fallback

A repository's `repository` slug is derived from its origin remote
(`host/owner/name`). A repository with no usable remote instead gets a
deterministic path-based fallback of the form `local/<absolute-path>` (path
separators normalized to `/`). Every surface that reports repository identity —
`kwt list --json`, `kwt list -g --json` discovery, and `kwt projects --json` —
resolves it through the same code, so the fallback is identical across all
three and the surfaces remain joinable even for local-only repositories.

## Workspace session bootstrap

Every workspace session kwt creates (`add`, `open`, and the TUI) applies the
same bootstrap so panes are indistinguishable regardless of which client
attaches. kwt sets `default-command` to the empty string, which tells tmux to
start its configured `default-shell` natively instead of passing a command
string through `$SHELL -c`. This supports valid non-POSIX shells such as fish
and tcsh and avoids running startup hooks in an extra non-login shell. For the
first pane, kwt queries the session's resolved `default-shell` and executes it
directly with `-l` after applying the session environment bootstrap. This
login-shell behavior is the parity mechanism across launchers; panes otherwise
see tmux's own `TERM_PROGRAM`/`TERM_PROGRAM_VERSION`, which tmux sets in every
pane regardless of what kwt does.

Session creation uses an inert first-pane placeholder to make that environment
ordering safe. `new-session` starts `sleep 2147483647` as separate argv words,
so no user shell or profile runs before the session exists. kwt then installs
the session remove-markers, resolves `default-shell`, and replaces the
placeholder with `<resolved-shell> -l`. Only after that respawn does it create
the remaining panes. Thus every real shell starts after launcher state has been
masked, and the first pane's startup files run exactly once.

kwt treats a canonical set of variables as launcher state — scoped to the
terminal, shell, or tool that launched kwt, not to the workspace session tmux
hosts — and applies it in two places that share a single definition, with one
explicit, documented exception, so they cannot silently drift apart:

- **Exec-time sanitization.** Every tmux invocation kwt makes execs tmux with
  these variables removed from its own environment, EXCEPT `EDITOR` and
  `VISUAL`. If no tmux server is running yet, the invocation that starts one
  is what seeds that server's GLOBAL environment table; sanitizing at exec
  time keeps launcher state out of that table in the first place, rather than
  only masking it later per session. (`PWD`/`OLDPWD`/`SHLVL`/`_` are included
  even though they aren't terminal-integration variables: worktree
  directories are passed to tmux via `-c`, and shells re-derive these on
  their own.) `EDITOR`/`VISUAL` are kept here because tmux itself reads them
  at server start to choose its default key mode (`status-keys`/`mode-keys`:
  vi vs. emacs), and a user's `tmux.conf` may consult them too; stripping
  them from the server's own exec environment would silently flip that
  behavior for every kwt-started server.
- **Session remove-markers.** The same variables — including `EDITOR` and
  `VISUAL`, with no exception — are also removed from each session with a
  session-scoped remove-marker (`set-environment -r`), which masks the
  global/server value for that session only without touching other sessions
  or the server-wide environment. This covers sessions created against an
  already-running server whose global table predates kwt's exec-time
  sanitization (e.g. a server another tool started), and it is what keeps
  `EDITOR`/`VISUAL` out of every pane's shell even though the server process
  itself now keeps them.

The full list: exact names `__CFBundleIdentifier`, `EDITOR`,
`KWT_FLEET_TOKEN`, `KWT_GITHUB_TOKEN`, `OLDPWD`, `PROMPT`, `PROMPT_COMMAND`,
`PWD`, `RPROMPT`, `SHLVL`, `TERM_PROGRAM`, `TERM_PROGRAM_VERSION`, `VISUAL`,
`WINDOWID`, `_`; and prefixes `ALACRITTY_`, `CONDA_`, `FZF_`, `ITERM`,
`KITTY_`, `NVM_`, `PYENV_`, `STARSHIP_`, `VIRTUAL_ENV`, `WEZTERM_`, `WT_`,
`VSCODE_`. Every kwt workspace also removes the exact variable named by
`fleet.token_env`, case-insensitively, both from tmux subprocesses and from
the session environment. Operational kwt variables such as `KWT_HOME` are
preserved. `EDITOR` and `VISUAL` are excluded from exec-time sanitization
only, per above; every other name in this list is treated identically by both
mechanisms.

`TERMINFO` is deliberately excluded from the whole list: it is functional
terminal configuration (a custom terminfo database path), not transient
launcher-integration state, and is needed for tmux attach rendering and for
pane applications resolving tmux's own `TERM`.

### Attaching from other tools

kwt applies this bootstrap when it _creates_ a session. A session that some
other tool created — for example with `tmux new-session -A -s <session_name>`,
which attaches if the session exists but otherwise creates it bare — starts
without the `default-command` and remove-markers, so its windows would not
match kwt's until repaired.

Two rules keep external tools consistent with kwt:

- **When the session already exists, attach only:** use `tmux attach-session -t
<session_name>` (or `switch-client -t` from inside tmux). Attach-only commands
  never create a bare session, so there is nothing to repair.
- **If your tool creates the session itself, apply the equivalent bootstrap:**
  set `default-command` to `""` and add a session-scoped remove-marker
  (`set-environment -r <name>`) for each launcher variable listed above
  (including `EDITOR`/`VISUAL` — the exec-time exception above applies only to
  how the tmux server process itself was started, not to the session
  remove-markers). To keep the first pane clean as well, create it with an
  inert direct-argv placeholder, install the markers, resolve the session's
  `default-shell`, and respawn that pane with the resolved shell and `-l`.

kwt is also self-healing here: the next time it attaches to a session it finds
already running, it re-applies the safe bootstrap subset (`default-command`
plus the remove-markers — never construction or pane commands), so a session
another tool created bare converges on consistent behavior for windows opened
after that attach.

PR imports use a stricter reuse boundary. Every import reports a deterministic,
workspace-specific socket. `pr import --start-session` and `pr attach` create
one blank shell session, record the canonical workspace path, and reuse only a
same-named session with that exact marker. They never execute configured layout
or agent commands. The isolated server starts without the provider or
configured fleet credential. The protected session also masks those names and
filters them from `update-environment`. Because tmux options are mutable,
`pr attach` enforces `attach-session -E` rather than trusting the current
option value.

The repair path deliberately does not rewrite panes in an externally created
session that is already running; it only makes future windows consistent. In a
session kwt creates itself, the inert-placeholder/respawn sequence also covers
the first pane, including when the tmux server was already running with
launcher variables in its global environment table.

## Exit behavior

Commands intended to launch the dashboard or attach to tmux require an
interactive terminal. In non-interactive contexts, use data-oriented commands
such as `list`, `status`, `get`, and `exec`.
