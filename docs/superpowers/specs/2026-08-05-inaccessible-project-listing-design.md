# Inaccessible Project Listing Design

## Problem

`kwt projects` currently emits every persisted project registry entry. When a
repository checkout is moved or deleted, clients receive the stale path and
cannot run repository-local inventory there. Ghosthub consequently presents a
workspace inventory warning even though a missing registered checkout should
be harmless discovery metadata.

Kwt cannot safely infer a repository's new filesystem location without
scanning. Automatic scanning would conflict with kwt's explicit-registration
model.

## Behavior

Both the table and JSON forms of `kwt projects` will omit a registered project
unless its configured path is an accessible Git repository. This includes
path-less entries: repository identity alone is not actionable inventory for
clients that need to inspect a local checkout. The command will continue
processing other entries and will exit successfully when some or all
registered paths are inaccessible.

The persisted registry will not be changed. Running kwt from the repository's
new location, or using `kwt projects add <new-path>`, will continue to update
the existing entry through its repository identity.

## Implementation

Project-list preparation will validate each path through kwt's existing Git
repository boundary before canonicalizing and emitting its repository
identity. Entries that fail validation are skipped. The identity fallbacks
that only served path-less or inaccessible entries will be removed rather than
retained as dead compatibility code. No filesystem scan, new CLI field,
fallback path, or registry cleanup behavior will be introduced.

## Testing and Documentation

A focused command test will provide one live repository, one missing path, and
one path-less entry, then assert that only the live repository is emitted. It
will also confirm that a fully filtered JSON result remains `[]`, not `null`.
The existing JSON and table contract tests will use live temporary
repositories so they remain valid under the accessibility requirement. The
obsolete path-less fallback test will be removed with the behavior it pins.
CLI documentation will state that inaccessible registered paths are omitted
and that registry entries remain available for later re-registration.
