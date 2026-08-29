# Homepage and changelog compaction design

## Goal

Use the first viewport to explain kwt and show credible adoption without making
the homepage feel like a large marketing pitch. Start the changelog with useful
release information instead of empty boilerplate.

## Homepage

Change the hero heading to `clamp(1.75rem, 3vw, 2.6rem)`. Keep the approved
definition and existing line
length so the heading remains the page's first element without dominating it.

Add one compact proof line directly below the Install and Quickstart buttons:

> kwt powers worktree management in Ghosthub, a native terminal for local and
> SSH-hosted sessions.

Link `Ghosthub` to `https://ghosthub.ai`. Keep the longer “Build on kwt” section
farther down the page because it explains the embedding relationship rather
than serving as the first proof point.

## Changelog

Remove the generic opening sentence and the empty `Unreleased` section. After
the page title, begin with `0.5.0` and its date. Restore an `Unreleased` section
only when it contains a real change.

## Scope and verification

Change only the homepage Markdown, homepage CSS, and changelog opening. Preserve
all release details, links, commands, and upgrade notes.

Run the Markdown formatter and strict Zensical build. Render the homepage and
changelog at desktop and 390-pixel mobile widths, then inspect the images for
hierarchy, wrapping, and overflow. Do not create the `v0.5.0` tag.
