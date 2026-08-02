# Releasing kwt

Releases are tag-driven. Do not create a GitHub Release by hand before the tag
workflow runs.

## Before tagging

1. Start from the exact commit on `main` that should be released.
2. Confirm the branch is clean and the intended CI checks passed.
3. Run `make test`, `make build`, and `make docs-check` locally.
4. Review the changes since the previous tag and choose the next semantic
   version. Because kwt is pre-1.0, user-visible contract changes normally
   require a minor version.

## Tag the release

Create and push an annotated tag without rewriting existing history:

```sh
git switch main
git pull --ff-only
release_version=vX.Y.Z # replace with the chosen semantic version
git tag -a "$release_version" -m "kwt $release_version"
git push origin "$release_version"
```

The release workflow tests the tagged commit, builds the supported target
matrix with GoReleaser, and publishes archives, checksums, and generated notes
to GitHub.

## Verify the result

On the [GitHub Releases](https://github.com/kenn-io/kwt/releases) page, confirm
that the release contains six archives plus `checksums.txt`. Download one
archive for the current platform, verify its checksum, and run:

```sh
kwt version
```

If the workflow fails, fix the cause on a new commit and tag a new version. Do
not move or replace a published version tag.
