# Worktree Maintenance

Worktree maintenance separates structural consistency repair from policies that
remove live worktrees. `kwt doctor` owns consistency; `kwt prune` owns explicit
live-removal policies.

## Inventory boundaries

Git's `worktree list --porcelain` output is the source of truth per repository.
Configured projects, discoverable repositories under the global worktree
directory, and existing registry paths provide bounded repository roots;
registry metadata and pull-request provenance are auxiliary joins. Registry
records with an active creation token are not roots because creation still owns
their incomplete state. Duplicate roots that resolve to the same Git common
directory produce one repository report.

Linked-worktree administrative directories are accepted only when their
`gitdir` file maps back to that exact worktree. A stale `.git` backlink to a
sibling worktree cannot supply generation or identity state. Repository-level
identity comes from the resolved main repository; linked-worktree origins are
retained separately for candidate-specific validation.

Inventory roots retain whether they came from configured projects. A configured
root requires a canonical configured repository identity; live-origin fallback
is reserved for unconfigured global and finalized-registry discovery.

Maintenance discovery is strict: traversal failures and unexpected `.git`
stat or read errors make the inventory incomplete instead of silently hiding a
subtree. Every discovered main or linked worktree is inspected as a repository
root and deduplicated by its canonical Git common directory. A linked root that
Git cannot inspect remains visible as a `project_unreachable` finding.

Maintenance inventory has two modes:

- Read-only inspection reads each existing `kwt-generation` file without
  creating one. Doctor and prune preflight use this mode, so diagnostics and
  dry-runs do not adopt legacy worktrees as a side effect.
- Ordinary operational listing may initialize a missing generation. This keeps
  normal `kwt list` behavior as the explicit adoption path for a verified live
  worktree.

The local inspector contains no provider logic. It gathers Git, filesystem,
registry, and provenance facts; merged-PR classification is a later provider
layer and completes before mutation starts.

## Structural repair

Doctor reports stable findings for broken or ambiguous backlinks, missing
worktree directories, stale registry entries, missing generations, unreachable
projects, repository identity mismatches, and moved or stale project
registrations. The default is read-only.
The JSON finding codes are `broken_worktree_backlink`,
`ambiguous_worktree_backlink`, `missing_worktree_directory`,
`stale_registry_entry`, `missing_generation`,
`registry_generation_mismatch`, `project_unreachable`, and
`repository_identity_mismatch`, `duplicate_registry_entry`, plus
`project_path_moved`, `stale_project_registration`, and
`ambiguous_project_relocation` for project configuration.

`doctor --fix` handles only uniquely identified structural state. For each Git
common directory it:

1. acquires that repository's worktree mutation lock;
2. rejects an active worktree-creation reservation;
3. revalidates the inspected path, Git directory, backlink, generation, and
   existence facts;
4. repairs uniquely owned backlinks only when every backlink Git could change
   is included in the validated fix scope;
5. inventories again and verifies that every record native Git pruning could
   remove is a uniquely fixable record in the expected snapshot;
6. invokes immediate native Git metadata pruning only when that complete scope
   remains valid.

Repair must precede pruning: a repaired backlink can make a moved live worktree
usable again, while pruning first could discard the administrative record that
repair needs. Locks are acquired and released one repository at a time; doctor
never holds multiple repository locks.

After mutation, doctor rescans and compares stable finding code/path pairs with
the pre-fix report. Human output lists resolved findings under **Fixed** before
the final inventory; JSON carries them in `fixed` and counts them in
`summary.fixed_findings`. `--quiet` suppresses the result report without
changing exit status and is mutually exclusive with `--json`.

Git may repair several backlinks in one repository even when given one linked
worktree path. Doctor therefore declines backlink repair for that repository if
any live repairable backlink is ambiguous or otherwise outside the validated
fix scope. This preserves every manual-only backlink unchanged while allowing
independent missing-record pruning and registry cleanup to continue.

Registry cleanup follows Git maintenance. Records with a valid generation use
generation-guarded removal. A confirmed-absent legacy record without a
generation uses full-entry compare-and-delete under the registry lock, so a
concurrent replacement is preserved. When a live registry record's generation
is missing, `doctor --fix` adopts the verified Git generation only by
compare-and-swap and clears any inherited expiration. A nonempty generation
mismatch identifies a possible replacement worktree; it remains a manual
conflict so policy metadata cannot transfer across generations. Inspection and
repair use the same canonical path identity, so a registry path reached through
an existing symlink still reconciles with its live Git worktree. Ambiguous
identities stay reported for manual review. Generation adoption rechecks the
inspected generation under the repository mutation lock and performs the
registry compare-and-swap inside that guard; a replacement becomes a no-op.
Registry records are excluded only
while another process holds their creation lock. A token left after its owner
exits is abandoned state: doctor inspects it and reacquires that path lock
before guarded reconciliation or removal. New expiration records store the
destination worktree's observed `origin` as a credential-free canonical
repository identity. Relative filesystem remotes are rejected. Doctor
accepts that identity when it matches either the configured project or the
verified live origin for the recorded worktree path. It normalizes legacy URL
values and renders unparseable legacy identities as `[redacted]` rather than
echoing raw remote data. Configured project identities follow the same output
boundary: canonical URL forms are rendered as credential-free identities,
while malformed values are omitted and reachable projects fall back to their
canonical live origin or local identity.

Registry entries are grouped by canonical filesystem path before individual
classification. Two raw paths that resolve to the same live worktree produce
one `duplicate_registry_entry` finding instead of being silently treated as
healthy. A group containing an actively owned creation token remains deferred
until creation finishes, consistent with other registry inspection. Otherwise,
the finding is fixable only when every repository value resolves to the same
credential-free identity, and branch, hash, main-worktree flag, registration
time, expiration, generation, and unreviewed-source policy are equal. Differing
or unsafe metadata remains a manual finding.

For a fixable group, inspection carries the complete observed entries as a
private repair condition. Registry mutation validates the complete alias group
under one registry lock, then replaces it with one unchanged entry. It prefers
the path spelling recorded by Git for the live worktree; if no raw entry uses
that spelling, it retains the lexicographically first path for deterministic
behavior. Any concurrent addition, removal, metadata change, or creation token
makes the transaction a no-op. This grouped compare-and-swap is required
because ordinary path-based registry operations intentionally reject multiple
keys that resolve to the same path.

Project repair starts only after an effective configured path is confirmed
absent. A credential-free repository identity may select exactly one
visible main repository as a relocation target. Multiple distinct matching
clones produce `ambiguous_project_relocation` and require manual choice; an
unsafe path-derived identity remains `project_unreachable`. When no matching
repository is visible, `stale_project_registration` authorizes removal of the
unchanged registration. If the sole matching live repository is already
registered, the stale entry is also a `stale_project_registration`; repair
removes the exact unchanged duplicate instead of relocating it onto the live
entry and creating two registrations for one path. If multiple missing
registrations independently select the same live target, all remain manual
`ambiguous_project_relocation` findings because kwt cannot safely choose which
registration metadata should survive.

Inspection keeps the raw project entry from global TOML beside its independently
expanded filesystem view. After Git and registry work is complete, `--fix`
rechecks that the old path is still absent and that a relocation target still
has the expected canonical main path, common directory, and repository
identity. It then performs a full-entry compare-and-swap under the shared
global-config writer lock. Relocation changes only the path and canonical
repository identity; removal deletes only the exact raw entry. Concurrent
edits and duplicate entries are no-ops. Unknown project fields are retained so
a newer kwt version can extend the entry without older repair code erasing its
metadata. The atomic writer resolves an existing `config.toml` symlink and
replaces its target, preserving the symlink and target permissions. The
repaired path is persisted as the verified absolute canonical main-repository
path. The same global-config transaction refuses a relocation if any other
current project entry already resolves to the target path. This lock-scoped
target guard prevents a concurrent registration—or an earlier repair from a
stale report—from creating duplicate project entries even when inspection saw
only one relocation claimant.

After every fix attempt, doctor reloads global configuration and the registry
before rescanning. Its report and exit status therefore describe persisted
post-fix state rather than the inspection that authorized the attempt. Human
output groups safe repairs before findings that need review, omits healthy
repositories and opaque machine evidence, and shortens paths only for display.
Versioned JSON retains exact paths and evidence.

## Conditional live removal

Expiration and merged-PR decisions are made outside the mutation lock, but a
decision never authorizes path-only deletion. The Git removal transaction
rechecks its supplied conditions under the repository lock immediately before
removal:

- the durable 32-character generation;
- expected HEAD when the policy depends on it;
- canonical repository identity when the policy depends on it;
- the linked worktree's local branch and exact upstream repository and branch
  when merged-PR evidence depends on them;
- cleanliness when required.

The transaction also rejects active creation reservations, main worktrees, and
worktrees that Git marks as locked. Dry-runs use the same lock-scoped inventory
and precondition validation as removals. A changed condition returns a typed
reason such as `generation_changed`, `head_changed`,
`repository_identity_changed`, `locked_worktree`, or `dirty_worktree`; the
worktree remains.

Merged-prune inventory carries the protected credential names derived from the
loaded global configuration into the removal transaction. Every Git process
that may execute worktree-selected configuration—including inventory, origin
and upstream reads, status checks, lock-time revalidation, and removal—runs
with kwt's GitHub and fleet credentials removed from its environment.

Generation-less or malformed-generation live worktrees are never automatically
removed. `--force` for expiration policy can relax cleanliness only. Ignored
artifacts do not make an expired worktree dirty. Merged-PR policy uses the
stricter rule that ignored untracked files are also local data, and it has no
force mode. Expiration removal compares the complete observed registry entry
while the Git mutation lock is held. Extending or clearing the expiration after
inspection preserves the worktree with `expiration_policy_changed`.

## Merged pull-request evidence

Imported worktrees fetch the recorded PR number and require the live workspace
path, branch, head SHA, source identity, project identity, canonical main
repository path, durable generation when recorded, and repository alias history
to match persisted provenance.
Workspace and project paths compare by canonical filesystem identity, so an
existing symlink alias is equivalent to its resolved path.

Ordinary worktrees derive source repository and branch from the configured Git
upstream as observed from the linked worktree, including worktree-specific Git
configuration. GitHub resolves the configured base repository before validating
imported alias history or looking up pull requests, so repository transfers do
not invalidate recorded aliases. It also resolves the observed upstream source
repository for provider matching and diagnostic head lookup while preserving
the observed identity for lock-time revalidation. GitHub's commit-associated PR
endpoint must return exactly one result whose base repository, source
repository, source branch, and head SHA match the local facts and whose
`merged_at` value is non-null. Exact PR head SHA survives squash and rebase
merges; local `git branch --merged` ancestry does not.
The configured project identity is the PR base even when the checkout's
`origin` points at a fork. The observed `origin` identity is read separately
from each linked worktree, retained apart from the PR base, and revalidated
under the repository lock before dry-run approval or removal. For a globally
discovered repository with no configured project, each candidate uses its own
observed `origin` as its PR base; sibling worktrees never share a fallback base.

That live-origin fallback is available only when every configured project root
was inspected successfully. If any configured root is unavailable, candidates
reached solely through global or registry discovery report `doctor_required`
before provider lookup. Healthy configured projects remain eligible. This
repository-wide guard is intentionally conservative: local evidence cannot
prove that a rediscovered fork is unrelated to the missing configured upstream,
so a merged pull request against the fork must not authorize removal. Running
`kwt doctor --fix` to relocate or remove the stale registration restores the
fallback on the next complete inspection.

An unavailable or noncanonical `origin` fails closed before provider lookup. An
expiration registry identity may match either this verified live origin or the
configured PR base, preserving existing origin-based records in fork clones.

When no exact commit result exists, source-head lookup is diagnostic only. It
distinguishes an advanced branch from no associated PR but cannot authorize
removal. Closed-but-unmerged PRs, multiple matches, missing upstream identity,
and provider failures preserve the worktree with stable reasons.

All candidate lookups finish before the first removal. A failure for one
candidate does not erase another candidate's confirmed result. After removal,
registry and imported-PR provenance cleanup use their observed generation or
complete value. New provenance records include the worktree generation, so a
same-path re-import cannot be deleted by cleanup for the removed incarnation; a
concurrent replacement produces `cleanup_incomplete` rather than being
overwritten. Every removal runs from the resolved main repository,
so deleting a globally discovered linked worktree cannot invalidate the Git
context needed to remove later candidates. Fleet state publishes once after any
actual removal, never for doctor, dry-runs, or no-ops.

## Command migration and statuses

The command split is intentional:

```text
kwt prune                 -> kwt doctor --fix
kwt prune --expired       -> live expiry policy; absent paths point to doctor
kwt prune --merged        -> clean, exact merged-PR policy
```

Doctor exits `0` when healthy, `1` when findings remain, and `2` when inspection
or fixes cannot complete. Prune exits `0` when confirmed candidates are handled
or absent, `1` for safety skips or per-candidate failures, and `2` for usage or
inventory failures. Both commands provide versioned JSON output.
JSON result reports are written to stdout without duplicating human result
summaries on stderr.

Prune reports one stable outcome for every considered path. Policy and safety
outcomes include `removed`, `would_remove`, `missing_generation`,
`doctor_required`, `dirty_worktree`, `head_advanced_after_pr`,
`no_associated_pr`, `pr_not_merged`, `source_repository_unavailable`,
`source_repository_mismatch`, `source_branch_mismatch`, `ambiguous_pr_match`,
`authentication_failed`, `network_failure`, `generation_changed`,
`expiration_policy_changed`, `head_changed`, `repository_identity_changed`, `main_worktree`,
`locked_worktree`, and `cleanup_incomplete`.
`no_associated_pr` and `pr_not_merged` are normal non-candidates and do not by
themselves make prune exit nonzero.
An individually discovered path that cannot be inspected becomes a
`doctor_required` outcome while other repositories continue. A strict discovery
error still aborts with exit `2` because the inventory is incomplete. A Git
removal that deregisters the worktree but leaves residual files completes
guarded registry and provenance bookkeeping, then reports `cleanup_incomplete`
with the residual path rather than treating the mutation as wholly failed.
Multiple registry entries that canonicalize to one candidate path also produce
`doctor_required`; merged pruning requires one unique registry snapshot before
it can authorize or clean up that worktree.
