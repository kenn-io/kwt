# Test Bootstrap Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sanitize Git, proxy, Go authentication, and kwt credentials before Go loads the root toolchain, root module, or test harness.

**Architecture:** A standard-library-only `tools/testbootstrap` nested module runs first and launches the existing root-module harness with a strict environment allowlist. The inner harness remains responsible for test-phase network denial and test-owned state. A nested-module integration test invokes the complete bootstrap-to-root-test path on Linux and Windows.

**Tech Stack:** Go 1.20 standard library for the bootstrap; Go 1.27 root module; GNU Make; GitHub Actions.

## Global Constraints

- The nested module uses exactly `go 1.20` and has no third-party dependencies.
- The root source-build requirement remains Go 1.27.
- Bootstrap dependency downloads use `GOPROXY=https://proxy.golang.org`, `GOSUMDB=sum.golang.org`, `GOAUTH=off`, `GOENV=off`, and `GOTOOLCHAIN=auto`.
- Ambient Git, proxy, `KWT_*`, private-module, and non-allowlisted variables do not reach the root `go run` process.
- The Windows CI job runs the behavioral bootstrap integration test.
- Tests exercise behavior; they do not assert workflow or source text.
- Remove this plan and its design spec before updating the public pull request.

---

### Task 1: Build and prove the dependency-free bootstrap

**Files:**
- Create: `tools/testbootstrap/go.mod`
- Create: `tools/testbootstrap/main.go`
- Create: `tools/testbootstrap/main_test.go`
- Modify: `internal/testharness/runner_test.go`

**Interfaces:**
- Produces: `bootstrapEnvironment(base []string, scratch string) []string`, which returns a unique, case-insensitive allowlisted environment with owned isolation values.
- Produces: `run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int`, which launches `go run ./internal/testharness/cmd -- <args>` from the repository root.
- Produces: `KWT_TEST_BOOTSTRAP=1`, an owned marker used only to activate the root behavioral probe.

- [ ] **Step 1: Write failing unit and behavioral tests**

Create `tools/testbootstrap/go.mod` with:

```go
module go.kenn.io/kwt/tools/testbootstrap

go 1.20
```

In `tools/testbootstrap/main_test.go`, add tests that exercise this shape:

```go
func TestBootstrapEnvironmentUsesStrictAllowlist(t *testing.T) {
    got := bootstrapEnvironment([]string{
        "PATH=/tools", "TEMP=/tmp", "SYSTEMROOT=C:\\Windows",
        "HTTP_PROXY=http://ambient", "GIT_CONFIG_GLOBAL=/ambient/gitconfig",
        "KWT_GITHUB_TOKEN=secret", "GOAUTH=netrc",
        "GOPRIVATE=private.example", "MY_TOKEN=secret",
    }, t.TempDir())
    values := environmentMap(got)
    for key, want := range map[string]string{
        "PATH": "/tools", "TEMP": "/tmp", "SYSTEMROOT": `C:\Windows`,
        "GOAUTH": "off", "GOENV": "off",
        "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org",
        "GOTOOLCHAIN": "auto", "GOVCS": "*:off",
        "KWT_TEST_BOOTSTRAP": "1",
    } {
        if values[key] != want { t.Errorf("%s = %q, want %q", key, values[key], want) }
    }
    for _, key := range []string{"HTTP_PROXY", "GOPRIVATE", "MY_TOKEN", "KWT_GITHUB_TOKEN"} {
        if _, ok := values[key]; ok { t.Errorf("%s unexpectedly remains", key) }
    }
}

func TestBootstrapRemovesAmbientVariablesEndToEnd(t *testing.T) {
    command := exec.Command("go", "run", ".", "--", "-run", "^TestBootstrapEnvironmentContract$", "./internal/testharness")
    command.Env = replaceEnvironment(os.Environ(), map[string]string{
        "GOPRIVATE": "private.example", "MY_TOKEN": "secret",
        "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOAUTH": "off",
    })
    output, err := command.CombinedOutput()
    if err != nil { t.Fatalf("bootstrap integration: %v\n%s", err, output) }
}

func TestBootstrapForwardsFailure(t *testing.T) {
    command := exec.Command("go", "run", ".", "--", "./internal/package-that-does-not-exist")
    if err := command.Run(); err == nil { t.Fatal("bootstrap returned success for a failing child") }
}

func environmentMap(entries []string) map[string]string {
    result := make(map[string]string, len(entries))
    for _, entry := range entries {
        key, value, ok := strings.Cut(entry, "=")
        if ok { result[strings.ToUpper(key)] = value }
    }
    return result
}

func replaceEnvironment(base []string, replacements map[string]string) []string {
    result := make([]string, 0, len(base)+len(replacements))
    for _, entry := range base {
        key, _, ok := strings.Cut(entry, "=")
        if _, replace := replacements[strings.ToUpper(key)]; ok && replace { continue }
        result = append(result, entry)
    }
    for key, value := range replacements { result = append(result, key+"="+value) }
    return result
}
```

Add this probe to `internal/testharness/runner_test.go`:

```go
func TestBootstrapEnvironmentContract(t *testing.T) {
    if os.Getenv("KWT_TEST_BOOTSTRAP") != "1" { t.Skip("requires the outer bootstrap") }
    for _, key := range []string{"GOPRIVATE", "MY_TOKEN"} {
        if _, ok := os.LookupEnv(key); ok { t.Fatalf("%s reached the test process", key) }
    }
    for key, want := range map[string]string{
        "GOAUTH": "off", "GOENV": "off",
        "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org",
        "GOTOOLCHAIN": "auto",
    } {
        if got := os.Getenv(key); got != want { t.Fatalf("%s = %q, want %q", key, got, want) }
    }
}
```

- [ ] **Step 2: Run the nested tests and verify they fail**

Run: `go -C tools/testbootstrap test ./...`

Expected: compilation fails because `bootstrapEnvironment` and the bootstrap executable do not exist.

- [ ] **Step 3: Implement the bootstrap**

In `tools/testbootstrap/main.go`:

```go
func main() {
    os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
    if len(args) > 0 && args[0] == "--" { args = args[1:] }
    cwd, err := os.Getwd()
    if err != nil { fmt.Fprintf(stderr, "test bootstrap: determine working directory: %v\n", err); return 1 }
    root := filepath.Clean(filepath.Join(cwd, "..", ".."))
    scratch, err := os.MkdirTemp("", "kwt-test-bootstrap-")
    if err != nil { fmt.Fprintf(stderr, "test bootstrap: create scratch directory: %v\n", err); return 1 }
    defer os.RemoveAll(scratch)
    if err = os.Chmod(scratch, 0o700); err != nil { fmt.Fprintf(stderr, "test bootstrap: protect scratch directory: %v\n", err); return 1 }
    if err = os.Mkdir(filepath.Join(scratch, "kwt"), 0o700); err != nil { fmt.Fprintf(stderr, "test bootstrap: create KWT_HOME: %v\n", err); return 1 }
    if err = os.WriteFile(filepath.Join(scratch, "gitconfig"), nil, 0o600); err != nil { fmt.Fprintf(stderr, "test bootstrap: create Git config: %v\n", err); return 1 }

    commandArgs := append([]string{"run", "./internal/testharness/cmd", "--"}, args...)
    command := exec.CommandContext(ctx, "go", commandArgs...)
    command.Dir, command.Env = root, bootstrapEnvironment(os.Environ(), scratch)
    command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
    if err = command.Run(); err == nil { return 0 }
    var exitErr *exec.ExitError
    if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 { return exitErr.ExitCode() }
    fmt.Fprintf(stderr, "test bootstrap: start test harness: %v\n", err)
    return 1
}
```

Build `bootstrapEnvironment` by retaining only these case-insensitive keys:

```go
var allowedEnvironment = map[string]struct{}{
    "APPDATA": {}, "CGO_ENABLED": {}, "COMSPEC": {}, "GOCACHE": {},
    "GOMODCACHE": {}, "GOARCH": {}, "GOPATH": {}, "GOROOT": {}, "GOOS": {},
    "HOME": {}, "HOMEDRIVE": {}, "HOMEPATH": {}, "LANG": {}, "LC_ALL": {},
    "LC_CTYPE": {}, "LOCALAPPDATA": {}, "PATH": {}, "PATHEXT": {},
    "PROGRAMDATA": {}, "SSL_CERT_DIR": {}, "SSL_CERT_FILE": {},
    "SYSTEMROOT": {}, "TEMP": {}, "TMP": {}, "TMPDIR": {},
    "USERPROFILE": {}, "WINDIR": {},
}
```

Use the allowlist and append the owned settings with this function:

```go
func bootstrapEnvironment(base []string, scratch string) []string {
    values := make(map[string]string, len(base))
    for _, entry := range base {
        key, value, ok := strings.Cut(entry, "=")
        if !ok { continue }
        key = strings.ToUpper(key)
        if _, allowed := allowedEnvironment[key]; allowed { values[key] = value }
    }
    result := make([]string, 0, len(values)+10)
    for key, value := range values { result = append(result, key+"="+value) }
    return append(result,
        "GOAUTH=off", "GOENV=off",
        "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org",
        "GOTOOLCHAIN=auto", "GOVCS=*:off",
        "GIT_CONFIG_GLOBAL="+filepath.Join(scratch, "gitconfig"),
        "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
        "KWT_HOME="+filepath.Join(scratch, "kwt"),
        "KWT_TEST_BOOTSTRAP=1",
    )
}
```

The owned settings are therefore exactly:

```text
GOAUTH=off
GOENV=off
GOPROXY=https://proxy.golang.org
GOSUMDB=sum.golang.org
GOTOOLCHAIN=auto
GOVCS=*:off
GIT_CONFIG_GLOBAL=<scratch>/gitconfig
GIT_CONFIG_NOSYSTEM=1
GIT_TERMINAL_PROMPT=0
KWT_HOME=<scratch>/kwt
KWT_TEST_BOOTSTRAP=1
```

- [ ] **Step 4: Format and run focused tests**

Run:

```sh
gofmt -w tools/testbootstrap/main.go tools/testbootstrap/main_test.go internal/testharness/runner_test.go
go -C tools/testbootstrap test ./...
go test ./internal/testharness
```

Expected: both test commands pass; the nested integration output proves the real child path completed.

- [ ] **Step 5: Commit the bootstrap behavior**

Commit the four implementation and test files with a message explaining that the isolation boundary must start before root module loading.

### Task 2: Route every supported test entry through the bootstrap

**Files:**
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`
- Modify: `.github/workflows/release.yml`
- Modify: `docs/development/contributing.md`

**Interfaces:**
- Consumes: `go -C tools/testbootstrap run . -- <go test arguments>`.
- Produces: one canonical invocation for local, CI, release, and focused test runs.

- [ ] **Step 1: Wire local and automation entry points**

Set `GO_TEST_RUNNER := go -C tools/testbootstrap run . --` in the Makefile. Make the canonical `test` target run `go -C tools/testbootstrap test ./...` before the full suite. Replace direct root-harness invocations in both CI matrix entries and the release workflow. Add a `Test bootstrap` step using `go -C tools/testbootstrap test ./...` before the normal test step in both workflows so the behavioral test executes on `windows-latest`.

- [ ] **Step 2: Update contributor guidance**

Replace focused `go run ./internal/testharness/cmd` examples with the nested bootstrap command. Explain that bootstrap module/toolchain preparation ignores ambient proxies, credentials, private-module settings, and module mirrors; it uses the public Go proxy. Keep the Git 2.32 requirement and describe the inner deny proxy separately.

- [ ] **Step 3: Exercise the supported entry points**

Run:

```sh
go -C tools/testbootstrap test ./...
go -C tools/testbootstrap run . -- ./internal/testharness
make test
```

Expected: all commands pass without an ambient credential or proxy dependency.

- [ ] **Step 4: Commit entry-point and documentation changes**

Commit the Makefile, workflows, and contributor documentation with a message explaining that all supported entry points now begin isolation before root compilation.

### Task 3: Verify portability and prepare the pull request

**Files:**
- Delete: `docs/superpowers/specs/2026-08-29-test-bootstrap-isolation-design.md`
- Delete: `docs/superpowers/plans/2026-08-29-test-bootstrap-isolation.md`
- Update: pull request 118 description only if its current summary omits the outer bootstrap outcome.

**Interfaces:**
- Consumes: the completed bootstrap and supported entry points.
- Produces: a clean public branch with no temporary planning documents.

- [ ] **Step 1: Verify cold-cache startup and Windows compilation**

Use a fresh temporary directory for empty `GOCACHE` and `GOMODCACHE`. Run only `TestBootstrapEnvironmentUsesStrictAllowlist` with outer `GOPROXY=off`, `GOSUMDB=off`, `GOAUTH=off`, and `GOTOOLCHAIN=local`; it must pass without loading the root module. Cross-build `tools/testbootstrap` for `windows/amd64` into that temporary directory.

- [ ] **Step 2: Run repository checks**

Run:

```sh
make test
make build
make docs-check
make lint
git diff --check
```

Expected: all checks pass.

- [ ] **Step 3: Remove temporary planning artifacts and commit**

Delete the spec and plan, verify `docs/superpowers` contributes no files to the branch diff, and commit the cleanup without rewriting earlier commits.

- [ ] **Step 4: Review, push, and update the pull request**

Review the complete diff against `origin/main`, scan public changes for private data, push the branch, and keep the pull request description concise and outcome-led. Do not tag, merge, comment on the review, or poll CI.
