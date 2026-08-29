# Test bootstrap isolation design

## Problem

The repository currently starts its test harness with `go run`. Before the
harness can sanitize its environment, Go may select or download a toolchain,
resolve modules, and compile the harness using ambient Git configuration,
proxy settings, and credentials. This leaves the documented isolation
boundary incomplete on a cold machine or module cache.

## Design

Add a small standard-library-only bootstrap program in a nested module with a
`go 1.20` directive. It can therefore sanitize the environment before the root
module's Go 1.27 toolchain or dependencies are loaded when the installed Go
version is 1.20 or newer. The documented source-build floor remains Go 1.27.

The bootstrap launches the existing `internal/testharness/cmd` entry point and
preserves its arguments, standard streams, working directory, and exit status.
Makefile targets, CI, release checks, and contributor documentation invoke the
bootstrap instead of invoking the harness directly.
The existing harness sanitization remains as a second layer.

## Environment contract

Construct the child environment from an allowlist of operating-system and Go
execution variables rather than copying `os.Environ`. In particular, do not
inherit:

- HTTP proxy variables or proxy credentials;
- Git configuration and credential variables;
- `KWT_*` variables and non-allowlisted custom token variables;
- Go authentication and private-module settings.

Set explicit child values for non-interactive, public dependency resolution:

- `GOAUTH=off`;
- `GOPROXY=https://proxy.golang.org` with no direct fallback;
- `GOSUMDB=sum.golang.org`;
- an empty global Git configuration with system configuration disabled;
- `GIT_TERMINAL_PROMPT=0`;
- a private temporary `KWT_HOME` owned and removed by the bootstrap.

Contributor documentation must state that test bootstrap downloads use the
public Go proxy and do not honor ambient private or regional module mirrors.

Keep only the platform, home/temp, executable lookup, certificate/locale, Go
cache, and Go toolchain variables required for Go to run cross-platform.
The outer allowlist is the credential boundary: the private `KWT_HOME` means
the existing harness cannot discover the caller's configured `fleet.token_env`.
The harness still removes kwt's built-in token names as a second layer.

This is an isolation contract against accidental ambient machine state. It is
not a security boundary against a contributor who can modify the repository's
bootstrap or workflows.

## Verification

Start with a behavioral integration test that invokes the real bootstrap under
a hostile environment. Use `GOPRIVATE` and an arbitrary `MY_TOKEN` as end-to-end
sentinels because the existing harness does not remove them and therefore
cannot mask a bootstrap failure. Unit tests cover proxy, Git, KWT, and Go
authentication filtering. Exercise exit-code and argument forwarding through
the same path.

Also verify the bootstrap can start from empty Go build and module caches
without loading the root module first, cross-build it for Windows, and run the
behavioral integration test in the Windows CI job. Do not use source-text
assertions.
