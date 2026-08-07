# Threat model

`kwt` is a local developer tool, not a sandbox for Git repositories. Security
reviews should distinguish data controlled by a branch from execution policy
already controlled by the machine's owner.

## Trusted local state

The following are trusted because changing any of them already gives the local
user or an attacker equivalent code-execution capability outside `kwt`:

- the operating-system account and its environment;
- executables on `PATH`, shell and tmux configuration, and agent commands;
- global `kwt` configuration;
- an existing repository's Git configuration, hooks, remotes, URL rewrites,
  credential helpers, filters, and fsmonitor configuration; and
- authenticated fleet peers belonging to the same user.

Consequently, ordinary Git inspection after a successful checkout—including
status, log, diff, ref discovery, and fleet metadata collection—is within the
accepted execution boundary. If trusted Git configuration deliberately invokes
a helper while performing those operations, `kwt` does not try to provide a
stronger sandbox than Git. Registry staleness that changes when inspection
occurs is a correctness concern, not a security boundary bypass.

## Untrusted inputs

Branch and ref names, pull-request metadata, remote repository contents, and
repository file contents are untrusted data. They must not be interpolated into
shell commands, environment-variable expansion, credentials, remote
destinations, or paths outside the user-selected or configured destination.

Repository-local `.kwt.toml` is separately trust-gated. Even a trusted local
file cannot set machine-level fleet credentials or endpoints. Trusting that
file authorizes its repository-scoped policy; it does not authorize branch
names or remote metadata to become shell syntax.

## Worktree creation and repository automation

Checking out a local or remote branch is an explicit user action and establishes
the same boundary as checking it out with Git directly. `kwt` still makes
creation unsurprising:

- ref and branch values are passed as arguments, never shell source;
- automated checkout disables configured hooks, filters, and recursive
  submodule updates;
- imported existing branches do not automatically run `copy_files`,
  `setup_commands`, layouts, or pane commands; and
- `kwt`-managed tokens are removed from checkout and workspace processes.

After checkout, the worktree participates in normal status, discovery, and
fleet observation. Opening it is the explicit opt-in to create its configured
workspace and run any trusted layout or pane commands.

## Secrets and machine-level policy

`kwt` bearer tokens, token-file locations, and credential-bearing remote URLs
must not be exposed to repository-controlled processes, printed, persisted in
shared state, or published to fleet. Fleet configuration is global-only, and
non-loopback fleet transport requires HTTPS.

Ordinary developer credentials and environment variables outside `kwt`'s own
credential surfaces remain the user's responsibility.

## Local daemon authority

The operating-system account is the local daemon trust boundary. The daemon
runtime directory and bearer record are owner-only because the credential can
authorize future worktree mutations and authenticated SSH use. Clients prove
the recorded endpoint before sending the bearer. The server accepts only its
exact loopback Host, rejects browser Origin requests, bounds bodies and
diagnostics, and never logs credentials or request bodies.

PID liveness alone does not authorize cleanup or replacement. Kwt also checks
the recorded process creation identity. A dead PID or exact creation mismatch
makes a runtime record stale; an identity that cannot be read, a timed-out
probe, or failed proof preserves the record and blocks a second writer.

## Out of scope

Do not report an issue as a `kwt` vulnerability when exploitation requires:

- a malicious binary already on the user's `PATH`;
- hostile shell, tmux, Git, hook, filter, fsmonitor, or credential-helper
  configuration already installed by the local user;
- another malicious process running as the same operating-system user;
- a compromised authenticated fleet machine; or
- treating a normal Git command after checkout as crossing an untrusted-code
  execution boundary.

These assumptions may still expose usability or correctness bugs. Severity
should follow the concrete consequence within the boundaries above: credential
disclosure, command injection by branch-controlled data, trust bypass, writes
outside configured roots, or pushes to the wrong repository are security
issues; stale UI state, compatibility, and ordinary Git behavior are not.
