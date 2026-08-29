# Releases

kwt uses semantic versions in `vMAJOR.MINOR.PATCH` form. It is still pre-1.0,
so minor releases may refine command or configuration contracts as the project
settles; upgrade notes call out changes that require attention.

## How releases work

Install v0.5.0 through Go:

```sh
go install go.kenn.io/kwt/cmd/kwt@v0.5.0
```

Semantic-version tags run the full test suite and publish a GitHub Release with
platform archives, generated release notes, and checksums. `v0.4.0` was the
first release produced by this automated artifact pipeline. `v0.3.0` was the
first versioned tag and predates packaged archives.

The latest published version and its notes live on
[GitHub Releases](https://github.com/kenn-io/kwt/releases). Install the newest
tag available through the Go module proxy with:

```sh
go install go.kenn.io/kwt/cmd/kwt@latest
```

See the [changelog](changelog.md) for the curated user-facing history.

## Choosing a version

- **Patch** releases contain compatible fixes and documentation corrections.
- **Minor** releases add capability or intentionally revise a pre-1.0 contract.
- **Major** releases are reserved for a stable contract with explicit breaking
  changes.

Every release binary reports the version, source commit, build time, Go version,
and target platform:

```sh
kwt version
```

Maintainers can find the tagging checklist in
[Releasing kwt](development/releasing.md).
