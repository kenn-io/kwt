# Install kwt into a shell-visible Go bin directory

## Problem

`make install` currently delegates the destination to the active Go toolchain's
`GOBIN`. When the repository is entered through mise, that can be a
toolchain-private directory that is only added to `PATH` in the kwt checkout.
Launching `kwt` from another repository can then resolve an older binary from
the user's ordinary Go bin directory. The result is that current source fixes
appear to be missing, including the dashboard startup improvements.

## Goals

- Make the repository's install target update the binary available to ordinary
  shells and sibling repositories.
- Keep the destination configurable for users with a different layout.
- Make the selected destination visible in install output and documentation.
- Verify the installed binary behavior without changing the user's installed
  binary during tests.

## Non-goals

- Changing shell startup files or editing a user's `PATH`.
- Removing arbitrary older binaries from other directories.
- Changing the behavior of `go install` when invoked directly.

## Design

Add an `INSTALL_DIR` Make variable whose default is the current Go `GOPATH`
bin directory. The `install` target will set `GOBIN=$(INSTALL_DIR)` for its
`go install` invocation, bypassing a toolchain-specific `GOBIN` while retaining
an explicit override such as `make INSTALL_DIR=/custom/bin install`.

The target will create the destination directory if needed and print the exact
installed path. Contributor documentation will explain that `make install`
uses the shared GOPATH bin directory and show how to verify resolution with
`command -v kwt` and `kwt --version`.

## Verification

The regression check will invoke `make INSTALL_DIR=<temporary directory>
install`, then execute the resulting binary and verify its version output.
This tests the target's observable behavior rather than inspecting Makefile
text. The regular Go tests, build, lint, vet, and the repository's formatting
checks will run before publication.
