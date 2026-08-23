# Worktree Change Inspection

Kwt has a local Git change engine for the transport-neutral Go inspection
service and the focused `kwt changes` command. It remains deliberately separate
from the lightweight status collector used by list, TUI, and fleet surfaces.
This note records the ownership and correctness boundaries between them.

## Shared core

`internal/status` owns local Git change facts. `CollectChanges` performs one
bounded `git status --porcelain=v2 -z --untracked-files=all` read and produces:

- NUL-delimited per-path index and working-tree states without a secondary
  line or whitespace delimiter;
- deterministic path ordering and original paths for renames and copies;
- mutually exclusive modified, added, deleted, untracked, and conflict counts;
- an orthogonal staged count that excludes unmerged index slots; and
- overall clean, modified, staged, or conflicted precedence.

The collector disables optional index writes, fixes the Git locale, bounds
stdout and stderr independently, and removes Kwt-protected credentials from
the Git process environment. Protected names include both Kwt's built-ins and
the effective global `fleet.token_env` value. Unknown NUL-framed porcelain
records are skipped for forward compatibility; malformed recognized records
still fail the snapshot. Compatible records with the same resulting path are
coalesced into one file entry while preserving independent index, worktree,
and original-path fields; incompatible duplicates fail the snapshot.

The Git adapter limits stdout and stderr to 1 MiB each. If an exact change list
exceeds the stdout limit, inspection never returns a partial snapshot. Its safe
public message is `worktree change list is too large to inspect`; the original
limit error remains available only to the in-process caller.

The module root aliases these values and `InspectionService`, so an in-process
consumer can use the same contract without Cobra, HTTP, or daemon ownership.

## Exact identity and generation fence

An inspection begins with current authoritative repository inventory for the
requested absolute path. Exactly one platform-canonical path must match, and
its repository identity, path, and durable generation must be complete.
Optional expected repository and generation values are compare-and-fail
guards, not selectors.

The service reads the durable generation from Git administration immediately
before and after collecting changes. A missing, moved, removed, or replaced
worktree returns retryable `registration_changed` and discards the file
snapshot. The fence prevents pairing a replacement checkout's files with an
older inventory identity. It does not claim branch or HEAD atomicity; the
result is a bounded local status observation. One five-second Git-operation
budget covers the pre-read, status collection, and post-read together. Git
process cleanup adds at most 100 milliseconds if a descendant retains an
output descriptor. Exhausting the internal budget returns retryable
`inspection_failed`; caller cancellation remains the caller's context error.

## CLI boundary

`kwt changes [path]` resolves the caller's literal path to an absolute path and
adapts `InspectionService`. The same-machine daemon supplies current inventory,
but the generation reads and Git status process stay in the invoking foreground
client. This preserves the foreground process's cancellation and credential
boundary while keeping the daemon free of status polling or file snapshots.

Human output is for direct inspection. The JSON `InspectionResult` is the
stable machine contract: authoritative worktree identity, canonical change
summary, deterministic file records, and observation time. Clean results retain
an explicit empty `files` array. JSON losslessly represents valid UTF-8 path
strings, including embedded whitespace and newlines; it cannot preserve an
ill-formed UTF-8 Unix filename byte-for-byte. Stable failures use the shared
service error envelope; prose messages are not part of the machine contract.

## Trust model

Ordinary Git inspection after checkout is inside Kwt's accepted local-user
trust boundary. Repository content remains data, while trusted machine-level
Git configuration may influence Git just as it does for a direct user command.
The inspection process does not expose Kwt-managed credentials, enable optional
index writes, approve repository-local Kwt configuration, or persist its
result. The foreground client requires config-bearing inventory, and the
inspection service fails closed before Git when effective configuration is
absent. Inventory ignores untrusted repository-local Kwt configuration for
this noninteractive read.

Native path comparison is platform-aware: existing symlink aliases converge on
Unix-like systems, and Windows comparison normalizes separators and case. Kwt
does not return a platform-specific unsupported result for inspection.

## Status collector boundary

`StatusCollector` retains its own one-shot porcelain-v2 snapshot, parser,
summary accounting, bounded workers, per-row diagnostics, remote state, and
activity calculation. Filtering, sorting, CSV, table, JSON, TUI, and fleet
responsibilities remain on their established paths and retain their schemas.
Detailed file records and the inspection service's semantic buckets are not
added to those surfaces.

The only shared integration is process isolation: every Git command owned by
`StatusCollector` receives the same built-in and configured protected-name set
from the CLI, TUI, or fleet caller. This keeps credentials out of foreground
and long-lived inventory processes without changing status accounting or TUI
presentation.

## Non-goals

Change inspection does not:

- fetch remotes or calculate ahead/behind state or activity;
- calculate diff, numstat, patches, or file contents;
- mutate the index or worktree;
- discover or batch sibling worktrees;
- watch, poll, cache, or persist change snapshots;
- move Git status ownership into the daemon;
- depend on tmux or workspace-session state; or
- introduce consumer-specific types or presentation policy.
