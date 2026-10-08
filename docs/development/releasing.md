# Releasing kwt

Coordinate publication with the release operator. This repository does not
publish release artifacts when a tag is pushed. Do not create a tag or GitHub
Release separately from the release process.

## Before publication

1. Start from the exact commit on `main` that should be released.
2. Confirm the branch is clean and the intended CI checks passed.
3. Run `make test`, `make build`, and `make docs-check` locally.
4. Review the changes since the previous tag and choose the next semantic
   version. Because kwt is pre-1.0, user-visible contract changes normally
   require a minor version.

## Release identity

Each release must use a new annotated semantic-version tag at the approved
source commit. Never move or replace a published tag or its artifacts.

Daemon build identity includes the full source revision and its source commit
time. The source time must be canonical RFC3339 UTC and must not be a package,
archive, or local build timestamp. Ordinary Go builds fall back to the embedded
`vcs.time`. Build systems that stamp a pinned revision explicitly, including
Ghosthub's managed helper build, must also pass its resolved source time:

```sh
-X go.kenn.io/kwt/internal/cmd.revisionTime=$revision_time
```

Release builds must stamp the same field from the tagged commit date.

## Verify the result

On the [GitHub Releases](https://github.com/kenn-io/kwt/releases) page, confirm
that the release contains archives for macOS, Linux, and Windows on AMD64 and
ARM64, plus a checksum file. Download one archive for the current platform,
verify its checksum, and run:

```sh
kwt version
```

If publication fails, preserve the tag and existing artifacts while the
release operator investigates. If the source needs a fix, merge it and use a
new version.
