# Pull-request automation

Use kwt to discover a GitHub pull request, import it into an isolated inert
worktree, inspect the checkout, and attach through its protected tmux boundary.
kwt preserves the contributor branch's exact push destination while keeping
repository setup and agent commands stopped until you choose to run them.

## Discover, import, inspect, and attach

List pull requests for a registered project:

```sh
kwt pr list --project github.com/acme/widget --state open --json
```

Pass the selected ID back to kwt to create the worktree:

```sh
kwt pr import github:github.com/acme/widget#17 \
  --project github.com/acme/widget --json
```

The result includes `workspace.path`. Review that exact checkout before running
project code:

```sh
kwt changes /path/from/import-result
```

Attach only through the protected PR command:

```sh
kwt pr attach /path/from/import-result
```

Direct `kwt open` and dashboard attachment refuse imported pull-request
workspaces. The protected command rechecks the live worktree and its recorded
project before it creates or repairs the isolated tmux session.

## Automation contract

kwt owns GitHub discovery, Git refs, worktree naming, push routing, and protected
session setup. A client should render the records returned by kwt and pass the
selected pull-request ID back. It should not reproduce those rules itself.

`kwt pr list` and `kwt pr import` are noninteractive and always emit JSON. The
examples keep `--json` explicit to document that the caller depends on the
automation contract.

Clients that present their own direct tmux client can ask kwt to establish
the canonical workspace session without attaching kwt's process:

```sh
kwt pr import 17 --project github.com/acme/widget \
  --start-session --json
```

Automation clients should bind the import to the project record that the user
selected. Supply the matching `repository` and `registration_fingerprint`
from `kwt projects --json` together:

```sh
kwt pr import 17 --project github.com/acme/widget \
  --expected-repository <project.repository> \
  --expected-registration <project.registration_fingerprint> \
  --start-session --json
```

Kwt revalidates both values while holding the project lifecycle fence and
before creating the worktree. A changed or replaced project returns the
retryable `registration_changed` error without importing anything. The two
flags are an all-or-nothing contract; ordinary interactive use may omit both.

Every successful import response includes `tmux_attach_mode: "protected"`.
When repository identity verifies the session name, it also includes the
deterministic `tmux_socket_name`. A protected result with no socket is protected
but unresolved: it is not the default tmux server, and clients must not attach
or create a session from that result. `--start-session` reports a
`session_start_error` for this state. `kwt pr attach` may resolve the endpoint
later, but only after it revalidates the live workspace provenance.

The socket name selects the endpoint; the attach mode selects Kwt's protected
attachment policy and must not be inferred from the presence of a named socket.
`--start-session` creates or repairs the returned `session_name` as a single
blank shell session and leaves it detached for the caller. It never runs
configured layouts, agent commands, or commands from the imported checkout.
Clients must attach through Kwt's protected path:

```sh
kwt pr attach <workspace.path>
```

Automation clients bind session establishment to current `kwt list --json` and
`kwt projects --json` snapshots. The worktree list is flat and does not contain
a nested project record or registration fingerprint. Match its `repository`
field to the project record with the same `repository`, then supply these five
values together:

| Attach flag               | Source                                                                   |
| ------------------------- | ------------------------------------------------------------------------ |
| `--expected-repository`   | Matching project's `repository` from `kwt projects --json`               |
| `--expected-registration` | Matching project's `registration_fingerprint` from `kwt projects --json` |
| `--expected-generation`   | Selected worktree's `generation` from `kwt list --json`                  |
| `--expected-session`      | Selected worktree's `session_name` from `kwt list --json`                |
| `--expected-socket`       | Selected worktree's `tmux_socket_name` from `kwt list --json`            |

```sh
kwt pr attach <workspace.path> \
  --expected-repository <project.repository> \
  --expected-registration <project.registration_fingerprint> \
  --expected-generation <workspace.generation> \
  --expected-session <workspace.session_name> \
  --expected-socket <workspace.tmux_socket_name>
```

Kwt revalidates those values while holding the project lifecycle fence and
before it creates or repairs the protected tmux session. A changed project,
worktree incarnation, session name, or socket returns the retryable
`registration_changed` error without touching tmux. The five flags are an
all-or-nothing contract; ordinary interactive use may omit all of them.

`kwt pr attach` is an interactive exception to the JSON automation contract.
Validation and session-establishment failures that occur before attachment
still use the structured error envelope and stable PR exit status. On
Unix-like systems, a successful handoff replaces `kwt` with tmux; from that
point onward, tmux owns terminal output, signal handling, and the final exit
status. An immediate tmux client failure after replacement therefore uses
tmux's native diagnostic and status rather than a JSON envelope. Windows
retains a waiting `kwt` parent and can still wrap a returned tmux failure.

The attach command resolves the persisted workspace identity, verifies the
recorded project clone and exact live worktree identity, and creates or repairs
a single blank shell session on its isolated server before executing
`attach-session -E` so tmux cannot import client environment variables. It
never runs configured layout or agent commands. This makes the command converge
imports created without `--start-session`, imports whose startup failed, and
sessions that disappeared after import. A parent tmux client identity is
removed before attachment, so the command also works when invoked from a pane
connected to another tmux server. This nests the protected client; detaching
returns to the outer server, and the shared prefix may need to be sent twice to
reach the inner client. A
deleted project, reused path, or branch, repository, or session-name mismatch
fails closed. Prunable entries and paths without a live Git worktree are not
accepted. The registered project's canonical identity remains authoritative
when its checkout's origin points to a fork, matching kwt's other inventory
surfaces. An import created from an unregistered repository remains attachable
when its live Git identity matches the recorded repository; ambiguous or
conflicting registrations fail closed. Direct `kwt open` and dashboard open
actions refuse imported workspaces because they use the normal tmux server;
use `kwt pr attach <workspace.path>` instead. The protected attach path is
idempotent for an already imported worktree or a verified session that kwt
created for the same workspace. If runtime session establishment fails after
the import becomes durable, the command still exits successfully and returns
the imported workspace with an explicit
`session_start_error`:

```json
{
  "status": "created",
  "workspace": {
    "path": "/worktrees/pr-17",
    "session_name": "kwt-wt-widget-pr-17-a1b2c3d4"
  },
  "session_start_error": {
    "code": "workspace_creation_failed",
    "message": "failed to start imported workspace session",
    "retryable": false
  }
}
```

Clients must retain or refresh the imported workspace when this field is
present and present the session failure separately. `kwt pr attach
<workspace.path>` retries session establishment directly; repeating the import
also converges on `already_imported`.

Kwt runs each protected PR workspace on a deterministic, workspace-specific
tmux socket rather than the shared dedicated `kwt` server or the
default server. Before invoking that server, kwt removes `KWT_GITHUB_TOKEN`,
`KWT_FLEET_TOKEN`, and the variable
named by `fleet.token_env` from the subprocess environment. It installs
matching session remove-markers before any imported-workspace shell starts.
Because tmux options are mutable by processes with socket access, filtering
`update-environment` is defense in depth; the protected attach command always
passes `-E` and never relies on that option for enforcement. Operational state
such as `KWT_HOME` remains available inside the workspace.

Kwt reuses an existing session on that isolated socket only when the session
carries the matching workspace marker and both its server and session
environments are credential-free. A rejected protected session must be
removed before retrying.

`kwt list --json` reads the same provenance store before it labels worktrees
with `tmux_socket_name` and `tmux_attach_mode`. If that store cannot be read or
decoded, listing fails instead of emitting an imported workspace without its
safety-critical endpoint and attachment policy.

`--project` accepts a repository identity from `kwt projects --json`, a
registered project name, or its absolute canonical main-repository path.
Identity and unique-name matching take precedence over path matching; relative
and symlinked path selectors are rejected. `--project` may be omitted when the
command runs inside the desired repository. If a display name identifies
multiple projects, kwt returns `repository_mismatch`; callers must use the
repository identity or path. `--state` accepts `open`, `closed`, or `all` and
defaults to `open`.

Before listing or importing pull requests, kwt resolves the selected repository
through GitHub and uses the current canonical identity for the provider request,
workspace, and provenance. GitHub repository transfers and renames therefore
continue to work when the project registry still contains the previous
identity. This resolution is operation-local and does not rewrite the
registered project, whose identity may intentionally name an upstream
repository while the checkout's origin points to a fork. Import selectors may
use either the registered or resolved repository URL during that transition.
Existing imports from the same project clone remain discoverable through the
verified alias pair; the next import migrates their repository, project,
same-repository source, workspace, and record-key identities to the resolved
repository atomically. Provenance retains the canonical alias history so later
transfers remain connected through a previously verified identity. Protected
attachment requires that history to overlap the live registered identity and
validates the recorded deterministic session against its own historical
repository identity.

## Authentication

GitHub API authentication is resolved without prompting:

1. `KWT_GITHUB_TOKEN`, when nonempty.
2. The output of `gh auth token`.

kwt uses that token only through `go-github`; it never writes or prints the
token. Git fetch and push use normal Git remote authentication. Import fetches
with `GIT_TERMINAL_PROMPT=0`, so missing Git credentials fail instead of
blocking an embedded or SSH client. Configure a Git credential helper or SSH
authentication for subsequent pushes.

Import stops before mutation when a repository stores credentials directly in
a remote fetch or push URL. Use a Git credential helper or SSH agent instead;
this keeps contributor-triggered Git operations from reading reusable
credentials out of the linked worktree's shared config.
Validation includes configured include files and checks Git's effective fetch
and push URLs after `insteadOf` and `pushInsteadOf` rewriting. Remote URLs with
query strings or fragments are rejected, as are invalid scheme-based URLs and
opaque remote-helper (`transport::address`) URLs.

Import fetches also force the SSH implementation's noninteractive mode
(OpenSSH batch mode or PuTTY/plink's equivalent) and disable askpass-style
credential prompts. Every ref-mutating import operation—including fetch,
checkout, and rollback—runs with the same sanitized environment and an empty
trusted hooks directory. Checkout additionally disables every configured
smudge/process filter. Repository hooks, filters, copied files, and setup
commands therefore do not run during PR import: environment scrubbing alone
cannot prevent same-user processes from reading kwt configuration or token
files from disk.

Kwt requires Git 2.20 or newer. PR import uses per-worktree Git configuration
to make plain `git push` target the PR head without changing push behavior in
the main checkout, and checks that capability before it fetches refs, adds
remotes, or creates a worktree. The `kwt doctor` command and explicit
`kwt prune` policies require Git 2.31 or newer for their worktree inventory and
repair operations.

## Listing contract

```json
{
  "pull_requests": [
    {
      "id": "github:github.com/acme/widget#17",
      "provider": "github",
      "repository": {
        "provider": "github",
        "identity": "github.com/acme/widget",
        "host": "github.com",
        "owner": "acme",
        "name": "widget"
      },
      "number": 17,
      "url": "https://github.com/acme/widget/pull/17",
      "title": "Improve widget rendering",
      "author": "octocat",
      "source": {
        "branch": "feature/rendering",
        "repository": {
          "provider": "github",
          "identity": "github.com/octocat/widget",
          "host": "github.com",
          "owner": "octocat",
          "name": "widget"
        },
        "is_fork": true
      },
      "target": {
        "branch": "main",
        "repository": {
          "provider": "github",
          "identity": "github.com/acme/widget",
          "host": "github.com",
          "owner": "acme",
          "name": "widget"
        },
        "is_fork": false
      },
      "draft": true,
      "state": "open",
      "head_sha": "0123456789abcdef0123456789abcdef01234567",
      "imported": false
    }
  ]
}
```

The opaque `id` is stable for the provider, base repository, and PR number.
Import also accepts a PR URL or a number scoped by `--project`.

An imported list result adds the canonical workspace record:

```json
{
  "id": "github:github.com/acme/widget#17",
  "provider": "github",
  "repository": {
    "provider": "github",
    "identity": "github.com/acme/widget",
    "host": "github.com",
    "owner": "acme",
    "name": "widget"
  },
  "number": 17,
  "url": "https://github.com/acme/widget/pull/17",
  "title": "Improve widget rendering",
  "author": "octocat",
  "source": {
    "branch": "feature/rendering",
    "repository": {
      "provider": "github",
      "identity": "github.com/octocat/widget",
      "host": "github.com",
      "owner": "octocat",
      "name": "widget"
    },
    "is_fork": true
  },
  "target": {
    "branch": "main",
    "repository": {
      "provider": "github",
      "identity": "github.com/acme/widget",
      "host": "github.com",
      "owner": "acme",
      "name": "widget"
    },
    "is_fork": false
  },
  "draft": false,
  "state": "open",
  "head_sha": "0123456789abcdef0123456789abcdef01234567",
  "imported": true,
  "workspace": {
    "id": "github.com/acme/widget:pr-17-feature-rendering:a1b2c3d4",
    "repository": "github.com/acme/widget",
    "branch": "pr-17-feature-rendering",
    "path": "/home/alice/.kwt/worktrees/github.com/acme/widget/pr-17-feature-rendering",
    "state": "ready",
    "session_name": "kwt-wt-widget-pr-17-feature-rendering-a1b2c3d4"
  }
}
```

## Import contract

kwt chooses a deterministic local branch name for the initial import and
delegates remote selection, fetch, no-checkout worktree creation,
materialization, and push routing to Kit. If the recorded worktree was removed
but its branch was preserved, another import creates a disambiguated branch
and updates the PR's provenance to the new worktree. The preserved branch is
left untouched. When the source branch is reachable, plain `git push` updates
exactly the PR's original head branch. If Kit cannot establish that exact fork
tracking, or KWT cannot validate it, import fails and rolls back the worktree
instead of leaving a checkout whose plain push could fall back to the base
repository. Unlike an ordinary `kwt add`, PR import does not apply `copy_files`
or `setup_commands`; run any desired project setup explicitly after reviewing
the imported files.
When Kit creates a fork remote, it preserves the project's working push
authentication transport, including SSH host aliases and explicit ports. KWT
then verifies the effective push destination and rejects broader push behavior
before reporting success. Import reports the exact tmux session name a client
can attach to; it does not launch or manipulate tmux panes.

Cross-project imports load the selected project's already trusted `.kwt.toml`
in isolation. They never load configuration from the caller's working
directory and never prompt or auto-trust in this automation path. Repository
`copy_files` and `setup_commands` are deliberately ignored for PR imports. A
target repository's `.kwt.toml` must be a
regular file, not a symlink, so trust granted to another path cannot be reused.
The registered project path must resolve to that repository's main Git root;
empty, relative, missing, subdirectory, and linked-worktree paths are rejected
before target configuration is loaded.

Configured file copies use rooted destination operations and reject symlinks
in the destination path, preventing contributor-controlled checkout entries
from redirecting writes outside the new worktree. Relative paths in a trusted
target `.kwt.toml` are resolved against that target repository, never the
caller's working directory, and target-local path fields cannot expand
environment variables into workspace paths. Naming output influenced by
target-local configuration is not environment-expanded after rendering.

Before materializing pull-request files, kwt creates a no-checkout worktree and
verifies that branch- and worktree-conditional Git includes do not change its
effective configuration, including record order and precedence. Push URLs and
refspecs are validated against the PR source repository again after push
configuration, and import fails if `HEAD` no longer names the generated
workspace branch. Worktree paths are stored and matched in canonical form so
symlinked base directories do not create duplicate imports.

A new import returns:

```json
{
  "status": "created",
  "pull_request": {
    "id": "github:github.com/acme/widget#17",
    "provider": "github",
    "repository": {
      "provider": "github",
      "identity": "github.com/acme/widget",
      "host": "github.com",
      "owner": "acme",
      "name": "widget"
    },
    "number": 17,
    "url": "https://github.com/acme/widget/pull/17",
    "title": "Improve widget rendering",
    "author": "octocat",
    "source": {
      "branch": "feature/rendering",
      "repository": {
        "provider": "github",
        "identity": "github.com/octocat/widget",
        "host": "github.com",
        "owner": "octocat",
        "name": "widget"
      },
      "is_fork": true
    },
    "target": {
      "branch": "main",
      "repository": {
        "provider": "github",
        "identity": "github.com/acme/widget",
        "host": "github.com",
        "owner": "acme",
        "name": "widget"
      },
      "is_fork": false
    },
    "draft": false,
    "state": "open",
    "head_sha": "0123456789abcdef0123456789abcdef01234567",
    "imported": true,
    "workspace": {
      "id": "github.com/acme/widget:pr-17-feature-rendering:a1b2c3d4",
      "repository": "github.com/acme/widget",
      "branch": "pr-17-feature-rendering",
      "path": "/home/alice/.kwt/worktrees/github.com/acme/widget/pr-17-feature-rendering",
      "state": "ready",
      "session_name": "kwt-wt-widget-pr-17-feature-rendering-a1b2c3d4"
    }
  },
  "project": {
    "identity": "github.com/acme/widget",
    "name": "widget",
    "path": "/home/alice/src/widget"
  },
  "workspace": {
    "id": "github.com/acme/widget:pr-17-feature-rendering:a1b2c3d4",
    "repository": "github.com/acme/widget",
    "branch": "pr-17-feature-rendering",
    "path": "/home/alice/.kwt/worktrees/github.com/acme/widget/pr-17-feature-rendering",
    "state": "ready",
    "session_name": "kwt-wt-widget-pr-17-feature-rendering-a1b2c3d4"
  }
}
```

Repeating the same import returns the same shape and workspace with
`"status": "already_imported"`. Provenance is stored in
`$KWT_HOME/pull-requests.json` (or `~/.config/kwt/pull-requests.json`) and is
updated under a cross-process file lock. The lock covers checking, fetching,
creating, configuring, and recording, so concurrent imports converge on one
workspace. A stale provenance record is not reported as imported when its Git
worktree no longer exists. Existing imports require complete source provenance
and matching project-clone, live worktree path, and branch identities before
`already_imported` is returned. Importing the same PR from another clone
returns a conflict rather than replacing the original clone's provenance. KWT
does not push during this check or rewrite Git remotes and routing that the
local user changed after import.

If the import transaction fails after creating a workspace, kwt rolls it back
even when the request context was canceled. Session establishment happens
after that transaction and therefore uses the partial-success contract above
rather than claiming rollback. Request cancellation, including `SIGINT` and
`SIGTERM`, terminates checkout; cleanup then runs without the canceled context.
Worktree creation retains an ownership reservation through late rollback and
removes only the original directory identity, a clean worktree, and the
unchanged reserved ref. A replaced path, dirty worktree, or advanced branch is
preserved and reported for manual cleanup. If the PR's recorded source
repository or branch changed while its imported workspace is still present,
another import returns `import_conflict`.

## Failure contract

Failures from `kwt pr list`, `kwt pr import`, and the validation or session
establishment phase of `kwt pr attach` write a JSON error to stdout, a
credential-free diagnostic to stderr, and return a stable nonzero status. The
Unix attachment handoff is the exception described above: after process
replacement succeeds, tmux owns any later diagnostic and exit status. For
example:

```json
{
  "error": {
    "code": "authentication_failed",
    "message": "GitHub authentication failed",
    "retryable": false
  }
}
```

| Exit | Error code                                     | Meaning                                                                                        |
| ---: | ---------------------------------------------- | ---------------------------------------------------------------------------------------------- |
|    2 | `invalid_pull_request_selector`                | Invalid state, URL, opaque ID, or number.                                                      |
|    3 | `authentication_failed`                        | GitHub API or Git authentication failed.                                                       |
|    4 | `repository_mismatch` / `unsupported_provider` | Project selection or provider mismatch.                                                        |
|    5 | `pull_request_not_found`                       | The selected PR or repository is missing.                                                      |
|    6 | `inaccessible_head`                            | The fork or source branch is unavailable.                                                      |
|    7 | `naming_conflict`                              | The generated branch or workspace is occupied.                                                 |
|    8 | `network_failure`                              | A retryable provider or Git network failure.                                                   |
|    9 | `workspace_creation_failed`                    | Worktree creation, setup, push config, persistence, or session-configuration preflight failed. |
|   10 | `malformed_provider_response`                  | GitHub returned an invalid success response.                                                   |
|   11 | `import_conflict`                              | Concurrent state or the selected head SHA changed.                                             |
|   12 | `unsupported_git_version`                      | Git is too old for isolated per-worktree push configuration.                                   |

GitHub primary and secondary rate limits, including HTTP 429 responses, use
`network_failure` with `retryable: true`.

All diagnostics go to stderr. Consumers should parse stdout and branch on
`error.code`; they never need to scrape CLI prose.
