# Project Registration Fingerprints

Date: 2026-08-11

Status: approved design

## Purpose

Bind project unregistration to the exact registry entry a client observed.
Exact path and credential-free repository identity remain required because they
identify the requested project, but neither detects an edit, removal and
recreation, or repurposing at the same path. An opaque registration fingerprint
adds that compare-and-swap evidence without exposing the persisted entry.

This contract must land before Ghosthub pull request #94 advances its pinned
kwt revision or ships. The daemonized project-removal API has not been released,
so this change defines the first shipped contract. There is no legacy fallback
or dual route.

## Goals

- Publish an opaque fingerprint for every registered project in current
  inventory.
- Require machine project-removal clients to submit the fingerprint they
  observed.
- Enforce the fingerprint inside the project lifecycle service, not only at the
  HTTP boundary.
- Preserve exact-path, repository-identity, protected-session, fencing, and raw
  config compare-and-swap checks.
- Preserve human CLI ergonomics without silently retrying a stale removal.
- Make response-loss reconciliation distinguish an unchanged registration from
  a replacement at the same path.
- Keep project inventory and all errors credential-safe.

## Non-goals

- The fingerprint is not a project identifier, authorization credential, or
  durable user-facing name.
- The fingerprint does not replace the exact path or expected repository
  identity.
- This change does not migrate another mutation to the daemon or introduce the
  durable-operation protocol.
- This change does not retain `project.removal.v1` compatibility.
- This change does not alter project-registration persistence or add an ID to
  `.kwt.toml`.

## Registration fingerprint

### Source value

The fingerprint input is `config.ProjectRegistration.raw`: the
`map[string]any` returned by Viper when it decodes one `[[projects]]` entry.
It is not the original TOML text. Viper has already discarded comments,
formatting, and key order and has normalized keys according to its decoding
rules.

The input must be the raw entry from a fresh global configuration snapshot.
Expanded paths, local repository inspection, canonical repository enrichment,
and merged repository-local configuration never participate in the
fingerprint.

Every semantic value present in the decoded raw entry participates, including
unknown future fields and `last_touched`. Consequently, a project touch after a
client observes inventory invalidates the fingerprint. This is intentional: a
recently changed or touched registration requires the client or user to refresh
and reconfirm removal.

### Canonical encoding

A dedicated version-one encoder converts the decoded value graph to bytes
before hashing. Its encoding is injective for every supported dynamic value:

- The stream begins with a domain and encoding-version marker specific to kwt
  project registrations.
- Each value begins with an explicit concrete type tag. Strings, booleans,
  signed and unsigned integer kinds, floating-point kinds, datetimes, maps, and
  arrays cannot share an encoding.
- Variable-length data is length-prefixed; concatenated values cannot be
  ambiguous.
- Map keys are string values, sorted by their exact UTF-8 bytes, and encoded
  with their values recursively.
- Arrays preserve element order and encode every element recursively.
- Integers preserve their concrete signedness and width. Floating-point values,
  including signed NaNs and their payloads, preserve their IEEE bit pattern.
  A NaN-bearing entry may pass the fingerprint comparison while the
  authoritative raw-entry comparison or final CAS loses because
  `reflect.DeepEqual` does not treat NaN as equal to itself. That direction is
  safe and retains the existing `registration_changed` behavior instead of
  making legal TOML data disable inventory.
- TOML datetime representations preserve every distinction relevant to the
  decoded value. A `time.Time` encodes its concrete type, wall-clock fields,
  nanoseconds, zone name, and UTC offset. It is never normalized to UTC.
  TOML local-date, local-time, and local-datetime values retain their concrete
  type and component fields.
- Nil values, non-string map keys, pointers, arbitrary structs, and any other
  unsupported dynamic values fail encoding rather than fall back to a textual
  representation.

The encoder's responsibility is injectivity; SHA-256 then provides
collision-resistant compression. The public token is the full digest with a
version prefix:

```text
v1:<64 lowercase hexadecimal digits>
```

Clients treat the token as opaque. They must not parse, derive, or compare its
contents except to retain and resubmit the complete string.

### Failure behavior

The global snapshot loader already treats a malformed raw project
representation as an inventory failure. Fingerprint derivation follows that
same fail-closed boundary. If any registered project cannot be encoded, the
entire inventory query fails; the project is not published without a token and
the TUI does not receive a partially authoritative project list.

This broad failure is deliberate. All legal TOML values, including NaN, have
canonical encodings, so a derivation failure indicates an implementation
invariant or unsupported decoder change rather than ordinary user data. The
private cause is logged through existing redaction; the public response remains
a sanitized stable error.

## Published project model

Inventory introduces a lifecycle-owned project DTO instead of adding a
response-only field to `pkg/models.Project`, which remains the persisted config
model. The DTO carries the existing JSON fields unchanged and adds one required
field:

```json
{
  "repository": "github.com/kenn-io/kwt",
  "name": "kwt",
  "path": "/code/kwt",
  "last_touched": "2026-08-11T12:00:00Z",
  "registration_fingerprint": "v1:..."
}
```

`Snapshot.Projects` changes from `[]models.Project` to the lifecycle-owned
type. The root `go.kenn.io/kwt` package exposes the type by alias alongside the
existing inventory contracts. CLI inventory, daemon transport, cache, TUI, and
reconciliation consumers move together; no compatibility alias or alternate
project list remains.

Every inventory view that publishes registered projects must derive them from
the same fresh global registration snapshot so it can attach authoritative
fingerprints. Repository identity enrichment remains a separate operation and
continues to produce the canonical credential-free `repository` field used by
both inventory and removal.

The project-removal success object retains its existing fields and does not
promise that the now-stale fingerprint remains usable after removal.

## Removal request and enforcement

`ProjectRemovalRequest` adds required JSON field `expected_registration`. The
complete conditional request is:

- `path`: exact persisted path, including trailing whitespace;
- `expected_repository`: credential-free identity from project inventory;
- `expected_registration`: opaque fingerprint from the same project record;
- `expansion`: the existing invoking-client expansion context.

The fingerprint is enforced in
`projectRemovalService.RemoveProject`, making the behavior identical for the
daemon endpoint and embeddable Go callers. The HTTP handler only decodes the
request and maps the service result.

The maintenance/doctor path already retains the inspected
`config.ProjectRegistration` as its private raw CAS token. Before calling the
same removal service, it derives `expected_registration` from that entry. The
derivation is tautological for its immediate inspection, but it preserves one
unconditional lifecycle request contract and leaves the later raw-entry
revalidation authoritative.

The service performs these checks in order:

1. Validate that path and expected repository are non-empty, the repository is
   a stable project identity, the fingerprint has the supported version and
   syntax, and the expansion context is valid. Invalid or missing fingerprints
   return non-retryable `invalid_request`.
2. Acquire the home-scoped project-transition fence.
3. Load a fresh global registration snapshot and select exactly one entry by
   byte-for-byte persisted path.
4. Derive the selected entry's fingerprint from its raw decoded map and compare
   the complete opaque token with `expected_registration`. A mismatch returns
   retryable `registration_changed` before identity locking, provenance reads,
   or tmux probes.
5. Resolve and compare the credential-free repository identity as today.
6. Acquire the repository-identity fence while retaining the transition fence.
7. Re-read the global snapshot and require the same raw entry and repository
   identity. The already existing `SamePersistedEntry` check remains the
   authoritative post-lock revalidation; no second fingerprint substitutes for
   it.
8. Release the transition fence, retain the identity fence, load authoritative
   protected endpoints, and fail closed for live or indeterminate endpoints as
   today.
9. Perform the final `CompareAndSwapProjectAt` using the raw registration from
   the revalidated snapshot. A lost CAS returns retryable
   `registration_changed`.

The fingerprint narrows the externally observed snapshot; the raw token,
fences, endpoint checks, and final CAS remain the mutation authority.

### Stable outcomes

| Condition | Code | Retryable |
| --- | --- | --- |
| Missing or malformed fingerprint | `invalid_request` | no |
| No exact persisted path | `project_not_found` | no |
| Duplicate exact persisted paths | `unregistration_failed` | no |
| Fingerprint mismatch | `registration_changed` | yes |
| Repository identity mismatch | `registration_changed` | yes |
| Post-lock raw-entry or identity mismatch | `registration_changed` | yes |
| Final config CAS loses | `registration_changed` | yes |
| Protected endpoint is live | `protected_session_live` | no |
| Protected endpoint authority is incomplete | `protected_endpoint_inventory_incomplete` | cause-specific |
| Metadata-only removal succeeds | existing `unregistered` result | n/a |

Messages never include raw registration data or credential-bearing repository
values. The digest is not accepted as authorization and is useful only with the
daemon bearer credential and the other conditional fields.

## CLI contract

The machine contract is explicit:

```text
kwt projects remove <exact-path> \
  --expected-repository <credential-free-identity> \
  --expected-registration <opaque-fingerprint> \
  --json
```

In `--json` mode both expected values are required. Missing either produces the
existing structured error envelope with `invalid_request` and exit code 2.
Ghosthub reads the path, repository, and fingerprint from one current
`kwt projects --json` record and passes all three unchanged.

In human, non-JSON mode, supplying neither expected flag causes the CLI to
request current project inventory, select the exact path, and fill both
expected values before sending the same conditional daemon request. Supplying
both flags sends them directly. Supplying only one is invalid. This avoids
asking a human to copy an opaque token without weakening the service contract.

The CLI never catches `registration_changed` by fetching a new fingerprint and
retrying. It reports that the registration changed and tells the user to review
current project inventory before trying again. A retry requires a new command
invocation or an explicit refreshed action in a UI.

Existing JSON success shape, error envelope, exit-code mapping, exact-path
semantics, and credential redaction remain unchanged except for the additive
project-inventory field and the new required machine flag.

## Transport-loss reconciliation

The daemon client reuses the request's expansion context to obtain current
project inventory after a project-removal response is lost. It compares the
exact path first, then repository identity and registration fingerprint:

- No exact path means the requested metadata is absent; synthesize the existing
  completed `unregistered` result.
- Exactly one entry with the same repository and same fingerprint means the
  observed registration remains; removal did not complete, so retain the
  transport failure.
- An entry at the exact path with a different repository or fingerprint means
  the registration changed; return retryable `registration_changed` and never
  report success.
- Duplicate matches or an unavailable current inventory leave the outcome
  indeterminate and retain the existing refresh-required transport behavior.

This reconciliation does not repeat removal and cannot remove a replacement.

## Daemon compatibility

The daemon continues to use API schema major 1 and the existing
`/api/v1/projects/remove` route. Its additive schema version advances from
`1.5.0` to `1.6.0`.

The project-removal capability changes from `project.removal.v1` to
`project.removal.v2`. The daemon advertises only v2 and the client requires
only v2:

- A new client refuses a stale daemon that advertises only v1.
- An old client refuses a new daemon that no longer advertises v1.
- A direct old request that omits the required fingerprint is rejected as
  `invalid_request`; it cannot fall through to path-and-repository removal.

No v1 handler, compatibility wrapper, optional fingerprint, or silent
client-side upgrade exists. Revision-aware daemon replacement remains the
normal way a newer client installs the v2 owner.

Ghosthub #94 must advance its revision-pinned bundled and managed helper only
after this kwt change merges. Its pinned contract suite must require the v2
capability, decode `registration_fingerprint`, pass it to removal, and cover a
same-path registration replacement before Ghosthub is released.

## Security properties

- Raw registration maps and their canonical encoding never cross the process
  boundary or enter public diagnostics.
- The full SHA-256 digest is emitted; no shortened collision budget is used.
- A fingerprint does not grant access and is checked only after authenticated
  daemon transport and lifecycle request validation.
- Credential-bearing stored repository URLs remain reduced to canonical
  credential-free identities in inventory and results. Tests retain a
  credential canary across stdout, stderr, HTTP errors, and daemon logs.
- Exact persisted paths, including trailing whitespace, remain unchanged in
  the request and comparison.
- Cached project records may carry fingerprints for display, but cached
  inventory remains non-authoritative and cannot enable TUI mutation. Current
  inventory is required before actions are enabled.

## Verification

Behavioral coverage must establish:

1. Reordered raw map keys produce the same fingerprint.
2. Distinct supported dynamic types, nested values, array order, unknown
   fields, and scalar values produce different fingerprints. Bounded generated
   supported value graphs additionally check that fingerprint equality tracks
   `reflect.DeepEqual`, subject only to the documented NaN behavior.
3. Datetimes representing the same instant with different wall offsets produce
   different fingerprints; datetime encoding never normalizes to UTC.
4. NaN sign and payload bits are encoded deterministically; unsupported
   non-TOML values still fail fingerprint derivation and fail inventory closed.
5. A `last_touched` change after observation returns retryable
   `registration_changed` without probing protected endpoints or changing the
   registry.
6. Path and repository equality cannot compensate for a fingerprint mismatch.
7. The lifecycle service rejects missing fingerprints for direct Go callers as
   well as HTTP callers.
8. Existing post-lock raw revalidation, protected-session rejection, and final
   CAS race tests continue to pass with the external fingerprint condition.
9. Response-loss reconciliation distinguishes same fingerprint, replacement
   fingerprint, absent entry, duplicate entry, and unavailable inventory.
10. `kwt projects --json` publishes a required fingerprint for accessible and
    missing checkouts without changing exact paths or credential-free
    repository identities.
11. JSON removal rejects a missing expected fingerprint with the stable error
    envelope and exit code 2; matching path, repository, and fingerprint
    succeeds.
12. Human removal resolves current evidence once, succeeds when unchanged, and
    reports rather than retries a concurrent change.
13. New clients refuse a v1-only daemon and old clients refuse a v2-only
    project-removal capability. Status reports schema version 1.6.0.
14. Inventory, removal success, all structured failures, stderr, and daemon
    logs never expose a credential canary from the persisted raw entry.
15. TUI and cache consumers compile against and preserve the lifecycle-owned
    project DTO; stale cache remains display-only.
16. Full tests, race tests for the affected lifecycle and daemon packages,
    lint, build, and Windows/Linux compilation pass.

## Delivery boundary

This work is one kwt pull request tied to kata `1sdd`. The local superpowers
specification and implementation plan are removed before the branch is pushed.
After merge, Ghosthub #94 advances its exact kwt revision and contract tests.
Only then is the current Ghosthub daemon-based project lifecycle ready for
release. Ordered daemon operation streaming (`jkzd`) remains the next kwt
prerequisite before migrating additional long-running mutations or SSH
lifecycle ownership.
