# Project Registration Fingerprints Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bind project unregistration to the exact raw registration observed by Ghosthub and other machine clients without weakening lifecycle fences or human CLI behavior.

**Architecture:** Canonically encode Viper's decoded raw project map and publish a versioned SHA-256 fingerprint in a lifecycle-owned inventory DTO. Require that token in the lifecycle removal service, carry it through CLI and HTTP, and use it to reconcile lost responses. Cut the unreleased capability directly from `project.removal.v1` to `project.removal.v2` with no fallback.

**Tech Stack:** Go 1.26, Viper-decoded TOML, SHA-256, Huma loopback HTTP, Cobra, `testing/quick`, Testify, and existing kwt config/lifecycle/daemon services.

---

## File map

- Create `internal/config/project_fingerprint.go` for canonical encoding, digest generation, and public-token validation.
- Create `internal/config/project_fingerprint_test.go` for encoder invariants, datetime/NaN behavior, and bounded generated value graphs.
- Modify `internal/lifecycle/inventory.go` and `internal/lifecycle/source.go` to publish a lifecycle-owned project DTO from raw global registrations.
- Modify `internal/lifecycle/cache.go` to roll the disposable inventory cache to version 2.
- Modify `internal/lifecycle/project_removal.go` and `internal/cmd/doctor.go` to enforce and derive the observed token.
- Modify `internal/cmd/projects.go` for the machine flag and one-shot human expectation resolution.
- Modify `internal/daemon/types.go` and `internal/daemon/client.go` for capability v2 and fingerprint-aware reconciliation.
- Update focused tests, subprocess contracts, and product documentation.
- Remove this plan and its design spec before opening the pull request.

### Task 1: Canonical raw-registration fingerprints

**Files:**
- Create: `internal/config/project_fingerprint.go`
- Create: `internal/config/project_fingerprint_test.go`
- Modify: `internal/config/config.go`

- [ ] **Step 1: Write failing canonicalization tests**

Create `internal/config/project_fingerprint_test.go` in package `config`. Construct `ProjectRegistration` values with private `raw` maps and cover map-order independence, dynamic-type separation, ordered arrays, nested maps, unknown fields, and token syntax:

```go
func TestProjectRegistrationFingerprintCanonicalizesDecodedValues(t *testing.T) {
	t.Parallel()
	first := ProjectRegistration{raw: map[string]any{
		"repository": "github.com/acme/widget",
		"path": "/repo ",
		"count": int64(5),
		"nested": map[string]any{"enabled": true, "items": []any{"a", int64(2)}},
	}}
	second := ProjectRegistration{raw: map[string]any{
		"nested": map[string]any{"items": []any{"a", int64(2)}, "enabled": true},
		"count": int64(5),
		"path": "/repo ",
		"repository": "github.com/acme/widget",
	}}

	left, err := first.Fingerprint()
	require.NoError(t, err)
	right, err := second.Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, left, right)
	assert.Regexp(t, "^v1:[0-9a-f]{64}$", left)
}

func TestProjectRegistrationFingerprintSeparatesDecodedTypes(t *testing.T) {
	t.Parallel()
	values := []any{int64(5), "5", float64(5), true, []any{int64(5)}, map[string]any{"v": int64(5)}}
	seen := map[string]struct{}{}
	for _, value := range values {
		fingerprint, err := (ProjectRegistration{raw: map[string]any{"value": value}}).Fingerprint()
		require.NoError(t, err)
		_, duplicate := seen[fingerprint]
		assert.False(t, duplicate, "value %#v collided", value)
		seen[fingerprint] = struct{}{}
	}
}
```

Also test that nil raw registrations, pointers, non-string map keys, and arbitrary unsupported structs fail rather than use a textual fallback.

- [ ] **Step 2: Add datetime and NaN tests**

Use two `time.Time` values representing the same instant with different offsets and require different fingerprints. Use `math.Float64frombits` to prove signed/payload NaNs encode deterministically and distinctly:

```go
func TestProjectRegistrationFingerprintPreservesDatetimeOffsetAndNaNBits(t *testing.T) {
	t.Parallel()
	instant := time.Date(2026, 8, 11, 12, 0, 0, 123, time.UTC)
	offset := instant.In(time.FixedZone("CDT", -5*60*60))
	utcToken, err := (ProjectRegistration{raw: map[string]any{"when": instant}}).Fingerprint()
	require.NoError(t, err)
	offsetToken, err := (ProjectRegistration{raw: map[string]any{"when": offset}}).Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, utcToken, offsetToken)

	left := math.Float64frombits(0x7ff8000000000001)
	right := math.Float64frombits(0xfff8000000000002)
	leftToken, err := (ProjectRegistration{raw: map[string]any{"value": left}}).Fingerprint()
	require.NoError(t, err)
	rightToken, err := (ProjectRegistration{raw: map[string]any{"value": right}}).Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, leftToken, rightToken)
}
```

- [ ] **Step 3: Add bounded generated-graph coverage**

Use `testing/quick` with a fixed seed and a test-local recursive generator limited to depth 3. Generate supported maps, slices, strings, booleans, signed integers, finite floats, and datetimes. Assert that `reflect.DeepEqual(left, right)` implies identical fingerprints and that a type-tagged mutation produces a different token. Keep the generator inside this test rather than adding a one-use package helper.

- [ ] **Step 4: Verify the red state**

Run:

```bash
go test ./internal/config -run 'TestProjectRegistrationFingerprint' -count=1
```

Expected: build failure because `ProjectRegistration.Fingerprint` and `ValidateProjectRegistrationFingerprint` do not exist.

- [ ] **Step 5: Implement the versioned encoder**

Create `internal/config/project_fingerprint.go` with:

```go
const projectRegistrationFingerprintVersion = "v1"

func (p ProjectRegistration) Fingerprint() (string, error) {
	if p.raw == nil {
		return "", errors.New("project registration has no raw persisted value")
	}
	var encoded bytes.Buffer
	encoded.WriteString("kwt.project-registration\x00v1\x00")
	if err := writeCanonicalProjectValue(&encoded, reflect.ValueOf(p.raw)); err != nil {
		return "", fmt.Errorf("encode project registration fingerprint: %w", err)
	}
	sum := sha256.Sum256(encoded.Bytes())
	return projectRegistrationFingerprintVersion + ":" + hex.EncodeToString(sum[:]), nil
}

func ValidateProjectRegistrationFingerprint(value string) error {
	version, digest, ok := strings.Cut(value, ":")
	if !ok || version != projectRegistrationFingerprintVersion ||
		len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return errors.New("expected registration fingerprint is invalid")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("expected registration fingerprint is invalid")
	}
	return nil
}
```

Implement `writeCanonicalProjectValue` using a concrete `reflect.Type` tag plus kind tag, uvarint-length-prefixed byte fields, sorted exact UTF-8 string map keys, ordered arrays/slices, exact integer width/signedness, and IEEE float bits. Special-case `time.Time` before generic struct rejection and encode wall-clock fields, nanoseconds, zone name, and offset without UTC normalization. Support TOML local date/time concrete structs by component fields. Reject nil interfaces, pointers, non-string keys, and unknown structs.

Do not use JSON, gob, `fmt.Sprint`, shortened digests, normalized UTC values, or fallback encoding.

- [ ] **Step 6: Run and format focused tests**

```bash
gofmt -w internal/config/project_fingerprint.go internal/config/project_fingerprint_test.go
go test ./internal/config -run 'TestProjectRegistrationFingerprint' -count=1
```

Expected: all fingerprint tests pass.

- [ ] **Step 7: Commit**

Run the mandatory `kenn:commit` workflow and commit only Task 1 files with subject:

```text
Derive exact project registration fingerprints
```

### Task 2: Publish authoritative project DTOs

**Files:**
- Modify: `internal/lifecycle/inventory.go`
- Modify: `internal/lifecycle/source.go`
- Modify: `internal/lifecycle/source_test.go`
- Modify: `internal/lifecycle/cache.go`
- Modify: `internal/lifecycle/cache_test.go`
- Modify: `internal/lifecycle/service.go`
- Modify: `kwt.go`
- Modify: `public_test.go`
- Modify: `internal/cmd/projects_test.go`

- [ ] **Step 1: Write failing inventory tests**

Extend `internal/lifecycle/source_test.go` so `ViewProjects` returns accessible and missing registrations with exact persisted paths, credential-free identities, and valid nonempty fingerprints. Change only `last_touched` or an unknown raw field between two loads and require a new token.

Add a `ViewRepository` test proving its `Snapshot.Projects` carries the same fingerprint as `ViewProjects`. This prevents fallback to typed/expanded `models.Project` data.

- [ ] **Step 2: Verify the red state**

```bash
go test ./internal/lifecycle -run 'TestSource.*Project|TestPublishedProject' -count=1
```

Expected: compile failure because the lifecycle project DTO and fingerprint field do not exist.

- [ ] **Step 3: Define the lifecycle DTO**

In `internal/lifecycle/inventory.go`:

```go
type Project struct {
	Repository              string `json:"repository"`
	Name                    string `json:"name"`
	Path                    string `json:"path"`
	LastTouched             string `json:"last_touched"`
	RegistrationFingerprint string `json:"registration_fingerprint"`
}

type Snapshot struct {
	Config        *models.Config     `json:"config"`
	Projects      []Project          `json:"projects"`
	Entries       []Entry            `json:"entries"`
	LaunchEntries []Entry            `json:"launch_entries,omitempty"`
	Workspaces    []models.Workspace `json:"workspaces"`
}
```

Add `Project = lifecycle.Project` to root aliases in `kwt.go`. Keep `pkg/models.Project` as the persistence and mutation-result type.

- [ ] **Step 4: Publish once from the raw global snapshot**

Change `publishedProjectRegistrations` to return `[]Project`, derive each `registration.Fingerprint()` before identity enrichment, and fail the query on any derivation error.

At the start of `currentSource.Load`, derive the published list once from `snapshot.Projects` and pass it into `loadRepository`. Use that same list for projects, global, dashboard, repository, forced-global, and non-repository fallback views. Continue using `snapshot.Config.Projects` for Git discovery and mutation settings.

Remove internal and root `CanonicalProjects` functions; typed models cannot produce authoritative tokens. Update command test fixtures to construct `[]kwt.Project` directly.

- [ ] **Step 5: Roll the disposable cache forward**

Set `cacheVersion = 2` and use `cache/inventory-v2.json`. Update existing cache tests. Do not read or migrate `inventory-v1.json`: stale cache is disposable and TUI mutations remain disabled until current inventory arrives.

- [ ] **Step 6: Run focused tests**

```bash
gofmt -w internal/lifecycle/inventory.go internal/lifecycle/source.go internal/lifecycle/source_test.go internal/lifecycle/cache.go internal/lifecycle/cache_test.go kwt.go public_test.go internal/cmd/projects_test.go
go test ./internal/lifecycle ./internal/cmd . -count=1
```

Expected: pass with authoritative fingerprints in every inventory view.

- [ ] **Step 7: Commit**

Run `kenn:commit` and commit Task 2 with subject:

```text
Publish observed project registration fingerprints
```

### Task 3: Enforce fingerprints in lifecycle removal and doctor

**Files:**
- Modify: `internal/lifecycle/project_removal.go`
- Modify: `internal/lifecycle/project_removal_test.go`
- Modify: `internal/cmd/doctor.go`
- Modify: `internal/cmd/doctor_test.go`

- [ ] **Step 1: Write failing lifecycle tests**

Update the removal fixture to derive its expected token from the initial raw snapshot. Add cases for missing/malformed tokens, a changed `last_touched` with the same path/repository, no endpoint probe on mismatch, and registry preservation. Matching tokens retain existing protected-session and final-CAS coverage.

- [ ] **Step 2: Write a failing doctor request test**

Replace `removeDoctorProjectRegistration` with a recorder, call `doctorProjectMutator.RemoveProject`, and assert `request.ExpectedRegistration` equals `expected.Fingerprint()`.

- [ ] **Step 3: Verify the red state**

```bash
go test ./internal/lifecycle ./internal/cmd -run 'TestProjectRemoval|TestDoctor.*Project' -count=1
```

Expected: compile failure for the absent request field, followed by failing validation tests.

- [ ] **Step 4: Add unconditional service enforcement**

Extend the request:

```go
type ProjectRemovalRequest struct {
	Path                 string           `json:"path"`
	ExpectedRepository   string           `json:"expected_repository"`
	ExpectedRegistration string           `json:"expected_registration"`
	Expansion            ExpansionContext `json:"expansion"`
}
```

At the beginning of `RemoveProject`, validate the token and map failure to non-retryable `service.InvalidRequest`. After the transition fence and exact-path selection, derive the daemon's own token. Derivation failure is internal. A mismatch returns retryable `service.RegistrationChanged` with message `the project registration changed after it was observed` before identity resolution, identity locking, provenance reads, or tmux probes.

Leave `SamePersistedEntry`, protected endpoint checks, and `CompareAndSwapProjectAt` authoritative and unchanged.

- [ ] **Step 5: Make doctor derive its token**

In `doctorProjectMutator.RemoveProject`:

```go
fingerprint, err := expected.Fingerprint()
if err != nil {
	return false, err
}
```

Pass it as `ExpectedRegistration`. Do not make the token optional for the maintenance-only seam.

- [ ] **Step 6: Run focused tests**

```bash
gofmt -w internal/lifecycle/project_removal.go internal/lifecycle/project_removal_test.go internal/cmd/doctor.go internal/cmd/doctor_test.go
go test ./internal/lifecycle ./internal/cmd -run 'TestProjectRemoval|TestDoctor.*Project' -count=1
```

Expected: pass, including no endpoint probe for stale observations.

- [ ] **Step 7: Commit**

Run `kenn:commit` and commit Task 3 with subject:

```text
Bind project removal to observed registration state
```

### Task 4: Finalize human and machine CLI behavior

**Files:**
- Modify: `internal/cmd/projects.go`
- Modify: `internal/cmd/projects_test.go`
- Modify: `internal/cmd/inventory_subprocess_test.go`

- [ ] **Step 1: Write failing CLI-mode tests**

Cover JSON missing-token exit 2, exact argument forwarding, one current human lookup when both expected flags are absent, invalid single-flag combinations, and no automatic refresh/retry after `registration_changed`. Use existing injected functions, not mock HTTP.

- [ ] **Step 2: Verify the red state**

```bash
go test ./internal/cmd -run 'TestRunProjectsRemove|TestProjectsRemove' -count=1
```

Expected: failures because the flag and human resolution path do not exist.

- [ ] **Step 3: Add the flag and validation matrix**

Bind:

```go
projectsRemoveCmd.Flags().StringVar(
	&projectsExpectedRegistration,
	"expected-registration",
	"",
	"Require the opaque registration fingerprint from projects --json",
)
```

Implement:

| Mode | Expected flags | Behavior |
| --- | --- | --- |
| JSON | both | send directly |
| JSON | one/both missing | `invalid_request`, exit 2 |
| Human | neither | load current exact-path project once and send its values |
| Human | both | send directly |
| Human | exactly one | `invalid_request`, exit 2 |

The human lookup uses `kwt.Request{View: kwt.ViewProjects, RequireCurrent: true}` and exact string equality. It never trims, normalizes, or silently retries.

- [ ] **Step 4: Update subprocess inventory coverage**

Decode project JSON as `[]kwt.Project`, validate the token, and pass both expected flags into removal. Preserve the bare top-level array and existing success/error shapes.

- [ ] **Step 5: Run and commit**

```bash
gofmt -w internal/cmd/projects.go internal/cmd/projects_test.go internal/cmd/inventory_subprocess_test.go
go test ./internal/cmd -run 'TestRunProjects|TestProjectsRemove|TestProjectsRemoveIsVisibleToDaemonInventory' -count=1
```

Expected: pass with one mutation attempt per invocation.

Run `kenn:commit` and commit with subject:

```text
Require observed registration evidence from machine clients
```

### Task 5: Cut daemon capability v2 and reconcile lost responses

**Files:**
- Modify: `internal/daemon/types.go`
- Modify: `internal/daemon/runtime.go`
- Modify: `internal/daemon/host.go`
- Modify: `internal/daemon/runtime_test.go`
- Modify: `internal/daemon/host_test.go`
- Modify: `internal/daemon/client.go`
- Modify: `internal/daemon/project_removal_test.go`
- Modify: `internal/cmd/daemon_client.go`
- Modify: `internal/cmd/daemon_subprocess_test.go`

- [ ] **Step 1: Write failing round-trip and reconciliation tests**

Update the exact-request round trip to assert `ExpectedRegistration`. Change lost-response fixtures to `[]kwt.Project` and cover absent-path success, unchanged-token transport failure, replacement-token `registration_changed`, replacement-repository `registration_changed`, and duplicate/unavailable inventory requiring refresh.

- [ ] **Step 2: Add failing capability coverage**

Require status to advertise `project.removal.v2` and schema 1.6.0. Use an observation fixture advertising only `project.removal.v1` and assert `daemon_incompatible` before removal. Verify a direct request missing the token returns `invalid_request`.

- [ ] **Step 3: Verify the red state**

```bash
go test ./internal/daemon ./internal/cmd -run 'TestProjectRemoval|Test.*Capability|Test.*Schema' -count=1
```

Expected: failures for old reconciliation and capability values.

- [ ] **Step 4: Implement fingerprint-aware reconciliation**

For one exact path:

```go
if !lifecycle.EqualProjectIdentity(project.Repository, request.ExpectedRepository) ||
	project.RegistrationFingerprint != request.ExpectedRegistration {
	return kwt.ProjectRemovalResult{}, false, service.NewError(
		service.RegistrationChanged,
		"the project registration changed while removal was being reconciled",
		true, nil, nil,
	)
}
return kwt.ProjectRemovalResult{}, false, nil
```

Treat duplicate exact paths as indeterminate and retain `refreshRequiredError`. Never repeat removal.

- [ ] **Step 5: Change capability and schema in place**

Set:

```go
APISchemaVersion         = "1.6.0"
CapabilityProjectRemoval = "project.removal.v2"
```

Keep schema major 1 and `/api/v1/projects/remove`. Advertise and require only v2. Define no v1 constant, optional token, second route, or adapter.

- [ ] **Step 6: Update credential-canary subprocess coverage**

Decode `[]kwt.Project`, pass `--expected-registration` in all removal cases, rewrite `last_touched` after observation, and assert `registration_changed`. Keep the credential canary absent from stdout, stderr, HTTP errors, and `daemon.log`.

- [ ] **Step 7: Run and commit**

```bash
gofmt -w internal/daemon/types.go internal/daemon/runtime.go internal/daemon/host.go internal/daemon/client.go internal/daemon/project_removal_test.go internal/cmd/daemon_client.go internal/cmd/daemon_subprocess_test.go
go test ./internal/daemon ./internal/cmd -count=1
```

Expected: pass with v2-only negotiation and precise reconciliation.

Run `kenn:commit` and commit with subject:

```text
Cut project removal over to fingerprint capability v2
```

### Task 6: Document and verify the first shipped contract

**Files:**
- Modify: `docs/design/daemon.md`
- Modify: `docs/development/threat-model.md`
- Modify: `docs/reference/cli.md`
- Modify: `docs/changelog.md`
- Modify: any focused test still compiling against `[]models.Project`

- [ ] **Step 1: Update product documentation**

Document exact path + credential-free repository + opaque fingerprint, lifecycle enforcement ordering, raw CAS authority, schema 1.6.0, `project.removal.v2`, `cache/inventory-v2.json`, one-shot human resolution, and no automatic retry. Add an Unreleased safety entry.

- [ ] **Step 2: Remove stale assumptions**

```bash
rg -n 'Snapshot\.Projects.*models\.Project|\[\]models\.Project.*Snapshot|project\.removal\.v1|inventory-v1\.json' --glob '*.go' --glob '*.md'
```

Expected: no stale current references. Historical immutable release text may remain. Convert only at publication boundaries; add no aliases or adapters.

- [ ] **Step 3: Format and run full gates**

```bash
make fmt
git diff --check
go test -race ./internal/config ./internal/lifecycle ./internal/daemon ./internal/cmd
make test
make lint
make build
make build-all
make docs-check
```

Expected: all commands exit 0, including Darwin/Linux/Windows builds.

- [ ] **Step 4: Commit**

Run `kenn:commit` and commit with subject:

```text
Document fingerprint-bound project removal
```

### Task 7: Remove local plans and hand off the Ghosthub pin

**Files:**
- Delete: `docs/superpowers/specs/2026-08-11-project-registration-fingerprints-design.md`
- Delete: `docs/superpowers/plans/2026-08-11-project-registration-fingerprints.md`

- [ ] **Step 1: Verify branch scope**

```bash
git status --short
git log --oneline --decorate origin/main..HEAD
```

Expected: implementation and planning commits only, with no unrelated changes.

- [ ] **Step 2: Delete and commit planning artifacts**

```bash
git rm docs/superpowers/specs/2026-08-11-project-registration-fingerprints-design.md
git rm docs/superpowers/plans/2026-08-11-project-registration-fingerprints.md
```

Run `kenn:commit` and commit with subject:

```text
Remove local registration fingerprint plans
```

- [ ] **Step 3: Run final clean-tree checks and close kata**

```bash
git diff --check origin/main...HEAD
git status --short
```

Expected: no output.

Close `1sdd` with the verified implementation commit:

```bash
kata close 1sdd --done --message "Project inventory now publishes an opaque fingerprint of each exact decoded raw registration; lifecycle removal requires it before identity/protected-endpoint work, response-loss reconciliation distinguishes replacements, and the daemon advertises project.removal.v2 only. Verified with focused race tests, full test/lint/build/docs gates, and cross-platform builds." --commit "$(git rev-parse HEAD)"
```

- [ ] **Step 4: Give Ghosthub the pin go-ahead**

Report the final kwt commit that Ghosthub #94 must pin. Ghosthub's contract suite must decode `registration_fingerprint`, require `project.removal.v2`, pass `--expected-registration`, and prove a same-path replacement returns `registration_changed`. Do not claim Ghosthub release readiness until that pin and contract update pass in #94.
