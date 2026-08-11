# Guarded Project Unregistration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let Ghosthub unregister an exact kwt project registration through the daemon only when the expected credential-free repository identity still matches and no protected tmux session can be orphaned.

**Architecture:** Project inventory and removal share one stable identity function over the same request-scoped expansion context. A daemon-owned removal service acquires an outer project lifecycle fence, reloads the exact raw registration, validates durable pull-request endpoint authority, performs a tri-state protected tmux probe, and finally uses the existing raw-entry configuration CAS. The same fence and registration claim guard registered-project worktree creation, pull-request import, and protected session establishment so removal cannot race a new inventory-owned endpoint into existence.

**Tech Stack:** Go 1.24, Cobra, Huma/HTTP, `gofrs/flock`, `go.kenn.io/kit/safefileio`, Git, tmux, TOML configuration CAS, Go unit/subprocess/race tests.

---

## File map

- Create `internal/lifecycle/project_identity.go`: stable remote-or-local project identity validation and publication.
- Modify `internal/worktree/worktree.go`: factor an exact-path local identity helper that does not trim a registered path.
- Create `internal/lifecycle/project_fence.go`: owner-private cross-process fence and registered-project claim/revalidation.
- Create `internal/lifecycle/project_removal.go`: exact registration selection, endpoint authority validation, probe policy, and metadata-only CAS.
- Create `internal/tmux/probe.go`: context-aware tri-state protected-session probe.
- Modify `internal/config/config.go`: expose home-scoped raw-entry CAS and remove the normalizing path-only unregister helper.
- Modify `internal/lifecycle/source.go`: retain missing/inaccessible registrations in project inventory while keeping worktree rows live-only.
- Modify `internal/lifecycle/inventory.go`: use registration-aware project publication without changing the JSON project model.
- Modify `kwt.go`: add only project-removal aliases and the constructor.
- Modify `service/error.go`: promote project mutation codes into the shared stable registry.
- Modify `internal/daemon/{types,runtime,host,server,client}.go`: capability, service construction, HTTP route, and typed client method.
- Modify `internal/cmd/{projects,daemon_client,project_guard,add,pr}.go`: daemon-only CLI removal and fence integration around creators.
- Modify `docs/reference/cli.md` and daemon design documentation: exact-path/identity contract, stable errors, and capability.

### Task 1: Stable project identities and dead-registration inventory

**Files:**
- Create: `internal/lifecycle/project_identity.go`
- Modify: `internal/worktree/worktree.go:834-855`
- Test: `internal/worktree/worktree_test.go`
- Modify: `internal/lifecycle/source.go:51-88,429-461`
- Modify: `internal/lifecycle/inventory.go:205-211`
- Test: `internal/lifecycle/source_test.go`

- [ ] **Step 1: Write failing tests for stable publication**

Add table-driven tests proving a missing checkout with a stored canonical identity stays in `Snapshot.Projects`, a missing local checkout receives a deterministic `local/...` identity, the published `Path` remains the exact persisted path, and a dashboard result does not synthesize `Snapshot.Entries` for the dead registration. Add a same-expansion-context test using a persisted `$PROJECT_ROOT/repo` path:

```go
func TestSourceProjectsRetainsMissingRegistrations(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing")
	writeGlobalProjects(t, home,
		models.Project{Repository: "github.com/acme/widget", Name: "widget", Path: missing},
		models.Project{Name: "local", Path: "$PROJECT_ROOT/local-missing"},
	)
	expansion := testExpansion(t)
	expansion.Environment["PROJECT_ROOT"] = filepath.Dir(missing)

	result, err := NewSource(SourceOptions{Home: home}).Load(context.Background(), Request{
		View: ViewProjects, Expansion: expansion, UntrustedConfig: IgnoreUntrustedConfig,
	})
	require.NoError(t, err)
	require.Len(t, result.Snapshot.Projects, 2)
	assert.Equal(t, "github.com/acme/widget", result.Snapshot.Projects[0].Repository)
	assert.Equal(t, missing, result.Snapshot.Projects[0].Path)
	assert.True(t, repositoryurl.IsLocalFallbackIdentity(result.Snapshot.Projects[1].Repository))
	assert.NotEmpty(t, result.Snapshot.Projects[1].Repository)
}

func TestDashboardDeadProjectIsInventoryOnly(t *testing.T) {
	// Configure one missing registration and an otherwise empty discovery root.
	// Assert it appears in Snapshot.Projects and no Entry names its path.
}

func TestProjectPublicationUsesRequestExpansionContext(t *testing.T) {
	// Send two requests with distinct PROJECT_ROOT values and assert the local/...
	// identity follows each request's effective path rather than daemon startup state.
}

func TestExactLocalIdentityPreservesTrailingWhitespace(t *testing.T) {
	info, err := internalworktree.RepositoryInfoFromExactLocalPath("/repo ")
	require.NoError(t, err)
	assert.NotEqual(t, "local/repo", info.FullPath)
	assert.Contains(t, info.FullPath, "repo ")
}
```

- [ ] **Step 2: Run the focused tests and confirm the old filtering behavior fails**

Run: `GOFLAGS=-buildvcs=false go test ./internal/lifecycle ./internal/worktree -run 'Test(SourceProjectsRetainsMissingRegistrations|DashboardDeadProjectIsInventoryOnly|ProjectPublicationUsesRequestExpansionContext|ExactLocalIdentityPreservesTrailingWhitespace)' -count=1`

Expected: FAIL because `CanonicalProjects` currently drops inaccessible paths and cannot preserve persisted path spellings.

- [ ] **Step 3: Add one identity implementation shared by publication and removal**

Create `internal/lifecycle/project_identity.go` with these exact contracts:

```go
package lifecycle

import (
	"fmt"
	"strings"

	"go.kenn.io/kwt/internal/config"
	repositoryurl "go.kenn.io/kwt/internal/url"
	internalworktree "go.kenn.io/kwt/internal/worktree"
)

func stableProjectIdentity(registration config.ProjectRegistration) (string, error) {
	if identity, ok := repositoryurl.CanonicalRepositoryIdentity(registration.Persisted.Repository); ok {
		return identity, nil
	}
	info, err := internalworktree.RepositoryInfoFromExactLocalPath(registration.Effective.Path)
	if err != nil || info == nil || !repositoryurl.IsLocalFallbackIdentity(info.FullPath) {
		return "", fmt.Errorf("derive local project identity")
	}
	return info.FullPath, nil
}

func validateStableProjectIdentity(identity string) (string, error) {
	if identity == "" || identity != strings.TrimSpace(identity) {
		return "", fmt.Errorf("expected repository identity is invalid")
	}
	if canonical, ok := repositoryurl.CanonicalRepositoryIdentity(identity); ok && canonical == identity {
		return identity, nil
	}
	if repositoryurl.IsLocalFallbackIdentity(identity) && identity != "local" {
		return identity, nil
	}
	return "", fmt.Errorf("expected repository identity is invalid")
}
```

Keep the actual implementation credential-safe: never include the rejected identity or stored remote in errors.

Factor `RepositoryInfoFromLocalPath` through a shared unexported implementation and add `RepositoryInfoFromExactLocalPath` with trimming disabled. Both helpers may make a path absolute and resolve an existing symlink, but the exact helper must not remove leading or trailing whitespace from a missing registered path.

- [ ] **Step 4: Publish registrations rather than only live Git repositories**

Replace the source's `CanonicalProjects` calls with a registration-aware helper:

```go
func publishedProjects(
	ctx context.Context,
	registrations []config.ProjectRegistration,
	protectedNames ...string,
) ([]models.Project, error) {
	projects := make([]models.Project, 0, len(registrations))
	for _, registration := range registrations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if registration.Persisted.Path == "" || registration.Effective.Path == "" {
			continue
		}
		identity, err := liveOrStableProjectIdentity(ctx, registration, protectedNames)
		if err != nil {
			return nil, err
		}
		published := registration.Persisted
		published.Repository = identity
		projects = append(projects, published)
	}
	return projects, nil
}
```

`liveOrStableProjectIdentity` may enrich an accessible checkout with its current credential-free canonical identity, but it must fall back to `stableProjectIdentity` for every Git/path error. Pass `snapshot.Projects` into this helper for global views. For repository-local merged views where raw registrations are unavailable, construct registrations whose persisted path is the configured spelling and effective path is the expanded spelling; do not make project publication failure depend on checkout access.

- [ ] **Step 5: Run lifecycle tests**

Run: `GOFLAGS=-buildvcs=false go test ./internal/lifecycle ./internal/worktree -count=1`

Expected: PASS, including updated coverage replacing the old “filters inaccessible registrations” expectation.

- [ ] **Step 6: Commit the inventory identity slice**

```bash
git add internal/lifecycle/project_identity.go internal/lifecycle/source.go internal/lifecycle/source_test.go internal/lifecycle/inventory.go internal/worktree/worktree.go internal/worktree/worktree_test.go
git commit -m 'Retain stable identities for unavailable projects'
```

### Task 2: Exact raw-entry CAS and the shared project fence

**Files:**
- Create: `internal/lifecycle/project_fence.go`
- Modify: `internal/config/config.go:837-946`
- Test: `internal/config/config_test.go`
- Test: `internal/lifecycle/project_fence_test.go`

- [ ] **Step 1: Write failing exact-path and fence tests**

Cover `/repo` versus `/repo `, duplicate exact paths, home-scoped CAS, a waiter canceled while another process owns the fence, and a stale registered-project claim rejected after the fence becomes available:

```go
func TestFindExactProjectRegistrationPreservesWhitespace(t *testing.T) {
	snapshot := snapshotWithProjects("/repo", "/repo ")
	match, count := findExactProjectRegistration(snapshot.Projects, "/repo ")
	require.Equal(t, 1, count)
	assert.Equal(t, "/repo ", match.Persisted.Path)
}

func TestProjectClaimRejectsRegistrationRemovedWhileWaiting(t *testing.T) {
	// Observe the claim, hold the same identity's fence, start AcquireAndValidate,
	// remove the exact raw entry, release the fence, and require
	// service.RegistrationChanged with no mutation callback invocation.
}
```

- [ ] **Step 2: Run the focused tests and confirm the APIs are absent**

Run: `GOFLAGS=-buildvcs=false go test ./internal/config ./internal/lifecycle -run 'Test(FindExactProjectRegistration|ProjectFence|ProjectClaim|CompareAndSwapProjectAt)' -count=1`

Expected: FAIL because the project fence, exact selector, and home-scoped CAS do not exist.

- [ ] **Step 3: Make configuration CAS explicitly home-scoped and delete path-only removal**

Refactor the existing implementation to:

```go
func CompareAndSwapProjectAt(
	home string,
	expected ProjectRegistration,
	replacement *models.Project,
) (bool, error) {
	configPath := filepath.Join(home, configName+"."+configType)
	// Preserve the existing raw map equality, one-match rule, mutation, and
	// viper refresh behavior; only the config path source changes.
}

func CompareAndSwapProject(
	expected ProjectRegistration,
	replacement *models.Project,
) (bool, error) {
	return CompareAndSwapProjectAt(getConfigDir(), expected, replacement)
}
```

Delete `config.UnregisterProject`; callers must not have a normalizing path-only removal entry point.

- [ ] **Step 4: Implement the owner-private project fence**

Create the fence directory and acquire one SHA-256-named lock with context-aware polling:

```go
type projectFence struct{ home string }

func (f projectFence) acquire(ctx context.Context, identity string) (func() error, error) {
	identity, err := validateStableProjectIdentity(identity)
	if err != nil {
		return nil, service.NewError(service.InvalidRequest, err.Error(), false, nil, err)
	}
	dir := filepath.Join(f.home, "project-locks")
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(identity))
	lock := flock.New(filepath.Join(dir, hex.EncodeToString(digest[:])+".lock"), flock.SetPermissions(0o600))
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !locked {
		return nil, errors.Join(err, ctx.Err())
	}
	return lock.Unlock, nil
}
```

Do not test `flock` or `safefileio` internals; test only kwt's naming, cancellation, and serialization contract.

- [ ] **Step 5: Implement an observed registration claim**

Keep generation/token machinery to one raw registration snapshot:

```go
type ProjectClaim struct {
	Registration config.ProjectRegistration
	Identity     string
	Expansion    ExpansionContext
}

func ObserveProjectClaim(
	snapshot *config.GlobalSnapshot,
	effectiveMainPath string,
	expansion ExpansionContext,
) (*ProjectClaim, error) {
	// Return nil when no registration owns the current repository; this is the
	// valid fresh-add path. Reject ambiguity. Record the raw registration and
	// stable identity without taking a lock.
}

func AcquireProjectClaim(
	ctx context.Context, home string, claim *ProjectClaim,
) (func() error, error) {
	if claim == nil {
		return func() error { return nil }, nil
	}
	release, err := (projectFence{home: home}).acquire(ctx, claim.Identity)
	if err != nil {
		return nil, err
	}
	current, err := config.LoadGlobalSnapshotAtWithExpansion(home, claim.Expansion.expandPath)
	if err != nil || !containsSameRegistration(current, claim.Registration, claim.Identity) {
		_ = release()
		return nil, service.NewError(service.RegistrationChanged,
			"the project registration changed before the operation began", true, nil, err)
	}
	return release, nil
}
```

The code must resolve repository-local configuration and trust before calling `AcquireProjectClaim`, and must not acquire the fence while holding Git, registry, provenance, configuration, or trust locks.

- [ ] **Step 6: Run fence and config tests under the race detector**

Run: `GOFLAGS=-buildvcs=false go test -race ./internal/config ./internal/lifecycle -run 'Test(FindExactProjectRegistration|ProjectFence|ProjectClaim|CompareAndSwapProjectAt)' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit the shared transaction primitive**

```bash
git add internal/config/config.go internal/config/config_test.go internal/lifecycle/project_fence.go internal/lifecycle/project_fence_test.go
git commit -m 'Add exact project lifecycle fencing'
```

### Task 3: Tri-state protected tmux probing

**Files:**
- Create: `internal/tmux/probe.go`
- Test: `internal/tmux/probe_test.go`

- [ ] **Step 1: Write parser and runner contract tests**

Use unit classification tests plus a focused real-tmux integration test. Cover exact attached and detached sessions as live; recognize absence only when exit status 1 carries tmux's explicit no-server/no-session stderr; classify permission, connection, missing-binary, cancellation, and every other failure as indeterminate; and classify successful output containing no exact session, malformed rows, or extra sessions as indeterminate:

```go
func TestProbeProtectedSessionClassifiesExactSession(t *testing.T) {
	tests := []struct {
		name string
		output string
		err error
		want ProtectedSessionState
	}{
		{"attached", "expected\t1\n", nil, ProtectedSessionLive},
		{"detached", "expected\t0\n", nil, ProtectedSessionLive},
		{"no server", "", "no server running on /tmp/tmux/socket", fakeExitError(1), ProtectedSessionAbsent},
		{"permission failure", "", "error connecting to /tmp/tmux/socket (Permission denied)", fakeExitError(1), ProtectedSessionIndeterminate},
		{"unexpected session", "other\t0\n", nil, ProtectedSessionIndeterminate},
	}
	// Assert state and whether an error is retained.
}
```

- [ ] **Step 2: Run the tests and confirm the probe is missing**

Run: `GOFLAGS=-buildvcs=false go test ./internal/tmux -run TestProbeProtectedSession -count=1`

Expected: FAIL to compile because the tri-state probe does not exist.

- [ ] **Step 3: Implement a context-aware named-socket probe**

Create:

```go
type ProtectedSessionState uint8

const (
	ProtectedSessionIndeterminate ProtectedSessionState = iota
	ProtectedSessionAbsent
	ProtectedSessionLive
)

type ProtectedSessionProbe interface {
	ProbeProtectedSession(context.Context, string, string) (ProtectedSessionState, error)
}

func ProbeProtectedSession(
	ctx context.Context, socketName, expectedSession string,
) (ProtectedSessionState, error) {
	command := NewTmuxCommandForSocket("", socketName)
output, stderr, err := command.RunCommandOutputContextWithStderr(
	ctx, "list-sessions", "-F", "#{session_name}\\t#{session_attached}",
)
return classifyProtectedSessionProbe(expectedSession, output, stderr, err)
}
```

Classify only tmux's documented no-server/no-session exit-1 diagnostics as absent. Exit status alone is never proof of absence. Preserve `ctx.Err()` with `errors.Join` when canceled. Require exactly one valid row naming the expected session on a successful command; unexpected successful server state is indeterminate. Add a real-tmux integration test that creates a protected server, proves both detached and control-client-attached sessions are live, proves a missing server/session is absent, and forces an exit-1 operational failure (for example an unusable `TMUX_TMPDIR`) that must remain indeterminate.

Define `fakeExitError` in the test as a tiny `error` plus `ExitCode() int` implementation. In production, classify through that interface so the real `*exec.ExitError` remains the implementation detail.

- [ ] **Step 4: Run tmux tests**

Run: `GOFLAGS=-buildvcs=false go test ./internal/tmux -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the probe**

```bash
git add internal/tmux/probe.go internal/tmux/probe_test.go
git commit -m 'Add authoritative protected tmux probes'
```

### Task 4: Guarded project-removal service

**Files:**
- Create: `internal/lifecycle/project_removal.go`
- Modify: `service/error.go:7-39`
- Modify: `kwt.go:10-75`
- Test: `internal/lifecycle/project_removal_test.go`

- [ ] **Step 1: Write service tests before defining the service**

Build temporary global configuration and provenance stores plus a fake probe. Cover exact whitespace success, not-found, duplicate exact path, expected-identity mismatch, CAS replacement, missing checkout with no provenance, attached/detached live rejection, indeterminate probe, malformed/incomplete/ambiguous provenance, and non-mutation of checkout/sentinel/provenance/default-server fixtures:

```go
func TestProjectRemovalRejectsLiveProtectedSession(t *testing.T) {
	fixture := newProjectRemovalFixture(t, "/repo ", "github.com/acme/widget")
	fixture.writeProvenance(validProvenance(fixture.project, fixture.workspace))
	fixture.probe.state = tmux.ProtectedSessionLive

	result, err := fixture.service.RemoveProject(context.Background(), ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget",
		Expansion: fixture.expansion,
	})

	assert.Empty(t, result.Project.Path)
	assert.True(t, service.IsCode(err, service.ProtectedSessionLive))
	assert.Equal(t, []string{fixture.expectedSocket}, fixture.probe.sockets)
	fixture.assertRegistrationPresent("/repo ")
	fixture.assertMetadataUntouched()
}

func TestProjectRemovalCASLossPreservesReplacement(t *testing.T) {
	// Replace the raw project entry in the hook immediately before CAS.
	// Require registration_changed and prove the replacement survives.
}
```

- [ ] **Step 2: Run the service tests and confirm failure**

Run: `GOFLAGS=-buildvcs=false go test ./internal/lifecycle -run TestProjectRemoval -count=1`

Expected: FAIL to compile because the project-removal contracts do not exist.

- [ ] **Step 3: Promote stable project error codes**

Add these exact `service.Code` constants:

```go
ProjectNotFound                     Code = "project_not_found"
RegistrationChanged                Code = "registration_changed"
UnregistrationFailed               Code = "unregistration_failed"
ProtectedSessionLive               Code = "protected_session_live"
ProtectedEndpointInventoryIncomplete Code = "protected_endpoint_inventory_incomplete"
```

- [ ] **Step 4: Define the root-facing removal contracts**

In `internal/lifecycle/project_removal.go` define:

```go
type ProjectRemovalRequest struct {
	Path               string           `json:"path"`
	ExpectedRepository string           `json:"expected_repository"`
	Expansion          ExpansionContext `json:"expansion"`
}

type ProjectRemovalResult struct {
	Project models.Project `json:"project"`
}

type ProjectRemover interface {
	RemoveProject(context.Context, ProjectRemovalRequest) (ProjectRemovalResult, error)
}

type ProjectRemovalServiceOptions struct{ Home string }

func NewProjectRemovalService(options ProjectRemovalServiceOptions) ProjectRemover {
	return newProjectRemovalService(options.Home, tmux.ProbeProtectedSession)
}
```

Alias only these four types and constructor from `kwt.go`; do not export fence, provenance, or tmux implementation types.

- [ ] **Step 5: Implement exact selection, durable authority, and final CAS**

The service method must follow this order:

```go
func (s *projectRemovalService) RemoveProject(
	ctx context.Context, request ProjectRemovalRequest,
) (result ProjectRemovalResult, resultErr error) {
	identity, err := validateStableProjectIdentity(request.ExpectedRepository)
	if err != nil || request.Path == "" || request.Expansion.validate() != nil {
		return ProjectRemovalResult{}, projectRemovalError(service.InvalidRequest,
			"project removal request is invalid", false, nil, err)
	}
	release, err := (projectFence{home: s.home}).acquire(ctx, identity)
	if err != nil {
		return ProjectRemovalResult{}, classifyProjectRemovalFailure(err)
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			resultErr = errors.Join(resultErr, projectRemovalInternal(releaseErr))
		}
	}()

	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(s.home, request.Expansion.expandPath)
	if err != nil {
		return ProjectRemovalResult{}, projectRemovalInternal(err)
	}
	registration, matches := findExactProjectRegistration(snapshot.Projects, request.Path)
	switch matches {
	case 0:
		return ProjectRemovalResult{}, projectRemovalError(service.ProjectNotFound,
			"no project is registered at the exact path", false, nil, nil)
	case 1:
	default:
		return ProjectRemovalResult{}, projectRemovalError(service.UnregistrationFailed,
			"multiple project registrations use the exact path", false, nil, nil)
	}
	actual, err := stableProjectIdentity(registration)
	if err != nil || actual != identity {
		return ProjectRemovalResult{}, projectRemovalError(service.RegistrationChanged,
			"the project registration no longer matches the expected repository", true, nil, err)
	}
	endpoints, err := s.loadProtectedEndpoints(ctx, registration, identity)
	if err != nil {
		return ProjectRemovalResult{}, classifyEndpointAuthorityError(err)
	}
	for _, endpoint := range endpoints {
		state, probeErr := s.probe(ctx, endpoint.SocketName, endpoint.SessionName)
		if state == tmux.ProtectedSessionLive {
			return ProjectRemovalResult{}, protectedSessionLive(endpoint)
		}
		if state != tmux.ProtectedSessionAbsent || probeErr != nil {
			return ProjectRemovalResult{}, incompleteEndpointProbe(probeErr)
		}
	}
	changed, err := config.CompareAndSwapProjectAt(s.home, registration, nil)
	if err != nil {
		return ProjectRemovalResult{}, projectRemovalInternal(err)
	}
	if !changed {
		return ProjectRemovalResult{}, projectRemovalError(service.RegistrationChanged,
			"the project registration changed before it could be removed", true, nil, nil)
	}
	project := registration.Persisted
	project.Repository = identity
	return ProjectRemovalResult{Project: project}, nil
}
```

`loadProtectedEndpoints` must copy the entire provenance map while its file lock is held, release that lock, then validate every relevant record. A missing store is an empty valid snapshot. This is safe because kwt commits provenance before establishing a protected session and rolls an import back if the provenance commit fails; manual deletion of kwt's owner-private provenance is outside the supported integrity model. Matching records require canonical project identity, effective clone path, nonempty workspace path, 32-character worktree generation, and nonempty exact session name; derive the socket with `tmux.ProtectedWorkspaceSocketName`. Malformed/unsupported/ambiguous records return non-retryable `protected_endpoint_inventory_incomplete`; lock/read failures and probe failures return the same code with `Retryable: true`. The live error details may contain only `session_name`, `socket_name`, and `generation`.

- [ ] **Step 6: Run service tests and the race suite**

Run: `GOFLAGS=-buildvcs=false go test -race ./internal/lifecycle -run TestProjectRemoval -count=1`

Expected: PASS.

- [ ] **Step 7: Commit the guarded service**

```bash
git add service/error.go kwt.go internal/lifecycle/project_removal.go internal/lifecycle/project_removal_test.go
git commit -m 'Add guarded project unregistration service'
```

### Task 5: Daemon capability, route, and typed client

**Files:**
- Modify: `internal/daemon/types.go:9-17`
- Modify: `internal/daemon/runtime.go:75-91`
- Modify: `internal/daemon/host.go:145-235`
- Modify: `internal/daemon/server.go:20-150,300-390`
- Modify: `internal/daemon/client.go:120-210,375-430`
- Test: `internal/daemon/server_test.go`
- Test: `internal/daemon/client_test.go`
- Test: `internal/daemon/runtime_test.go`
- Test: `internal/daemon/host_test.go`

- [ ] **Step 1: Write daemon round-trip tests**

Add tests that assert the sorted capability is advertised in runtime metadata/status, the route decodes the exact whitespace path and expected identity, success preserves the current CLI project JSON fields, and every project-removal error round-trips code/message/retryable/safe details without credentials:

```go
func TestProjectRemovalRoutePreservesExactRequestAndOutcome(t *testing.T) {
	remover := &recordingProjectRemover{result: kwt.ProjectRemovalResult{
		Project: models.Project{Repository: "github.com/acme/widget", Path: "/repo "},
	}}
	client := newAuthenticatedTestClient(t, ServerOptions{ProjectRemover: remover})
	result, err := client.RemoveProject(context.Background(), kwt.ProjectRemovalRequest{
		Path: "/repo ", ExpectedRepository: "github.com/acme/widget", Expansion: testExpansion(t),
	})
	require.NoError(t, err)
	assert.Equal(t, "/repo ", remover.request.Path)
	assert.Equal(t, "/repo ", result.Project.Path)
}
```

- [ ] **Step 2: Run the daemon tests and confirm the route/capability are absent**

Run: `GOFLAGS=-buildvcs=false go test ./internal/daemon -run 'Test(ProjectRemoval|Runtime.*Capabilities|Host.*Capabilities)' -count=1`

Expected: FAIL because the capability, server option, route, and client method do not exist.

- [ ] **Step 3: Add the capability and service construction**

Use:

```go
const (
	APISchemaVersion        = "1.5.0"
	CapabilityProjectRemoval = "project.removal.v1"
)
```

Keep both capability lists sorted exactly:

```go
[]string{
	CapabilityShutdown,
	CapabilityStatus,
	CapabilityProjectRemoval,
	CapabilityInventory,
	CapabilityRemoval,
}
```

Add `ProjectRemover kwt.ProjectRemover` to `HostOptions`/`ServerOptions`, create `kwt.NewProjectRemovalService(kwt.ProjectRemovalServiceOptions{Home: opts.Home})` when nil, and pass it into `NewServer`.

- [ ] **Step 4: Register the one daemon mutation route**

Add:

```go
type projectRemovalInput struct{ Body kwt.ProjectRemovalRequest }
type projectRemovalOutput struct{ Body kwt.ProjectRemovalResult }

huma.Register(api, huma.Operation{
	Method: http.MethodPost, Path: "/api/v1/projects/remove",
	OperationID: "project-remove",
}, func(ctx context.Context, input *projectRemovalInput) (*projectRemovalOutput, error) {
	release, err := reserveInventoryWork(opts)
	if err != nil {
		return nil, reportProblem(opts, "/api/v1/projects/remove", err)
	}
	defer release()
	result, err := opts.ProjectRemover.RemoveProject(ctx, input.Body)
	if err != nil {
		return nil, reportProblemWithExpansion(
			opts, "/api/v1/projects/remove", err, input.Body.Expansion,
		)
	}
	return &projectRemovalOutput{Body: result}, nil
})
```

Map `project_not_found` to HTTP 404, `registration_changed` and `protected_session_live` to 409, and `protected_endpoint_inventory_incomplete` to 503 only when retryable (otherwise 409). Allow only the three documented live-endpoint detail keys across this route.

- [ ] **Step 5: Add the client method and recognized codes**

```go
func (c *Client) RemoveProject(
	ctx context.Context, request kwt.ProjectRemovalRequest,
) (kwt.ProjectRemovalResult, error) {
	var result kwt.ProjectRemovalResult
	err := c.request(ctx, http.MethodPost, "/api/v1/projects/remove", request, &result, mutationResponseLimit)
	return result, err
}
```

Add all five project codes to `problemCode`. Do not add a direct fallback or another route.

- [ ] **Step 6: Run daemon tests**

Run: `GOFLAGS=-buildvcs=false go test -race ./internal/daemon -count=1`

Expected: PASS.

- [ ] **Step 7: Commit the daemon contract**

```bash
git add internal/daemon/types.go internal/daemon/runtime.go internal/daemon/host.go internal/daemon/server.go internal/daemon/client.go internal/daemon/server_test.go internal/daemon/client_test.go internal/daemon/runtime_test.go internal/daemon/host_test.go
git commit -m 'Serve guarded project removal through the daemon'
```

### Task 6: Daemon-only `projects remove` CLI contract

**Files:**
- Modify: `internal/cmd/projects.go:17-70,213-255`
- Modify: `internal/cmd/daemon_client.go`
- Test: `internal/cmd/projects_test.go`
- Test: `internal/cmd/daemon_subprocess_test.go`

- [ ] **Step 1: Write CLI tests for exact arguments and JSON**

Cover required `--expected-repository`, trailing-whitespace preservation, success shape, every stable error, credential redaction, and capability refusal. The injected client must observe the exact positional path:

```go
func TestProjectsRemovePassesExactPathAndExpectedRepository(t *testing.T) {
	projectsExpectedRepository = "github.com/acme/widget"
	removeProjectThroughDaemon = func(_ context.Context, request kwt.ProjectRemovalRequest) (kwt.ProjectRemovalResult, error) {
		assert.Equal(t, "/repo ", request.Path)
		assert.Equal(t, "github.com/acme/widget", request.ExpectedRepository)
		assert.NotEmpty(t, request.Expansion.HomeDirectory)
		return kwt.ProjectRemovalResult{Project: models.Project{
			Repository: "github.com/acme/widget", Path: "/repo ", Name: "widget",
		}}, nil
	}
	err := runProjectsRemove(commandWithBuffers(t), []string{"/repo "})
	require.NoError(t, err)
}
```

Add a subprocess assertion that a credential-shaped expected repository never appears in stdout or stderr.

- [ ] **Step 2: Run the CLI tests and confirm the direct helper still owns removal**

Run: `GOFLAGS=-buildvcs=false go test ./internal/cmd -run 'TestProjectsRemove|TestDaemonSubprocess.*Project' -count=1`

Expected: FAIL because the flag and daemon request do not exist.

- [ ] **Step 3: Make the command require identity and call only the daemon**

Register:

```go
projectsRemoveCmd.Flags().StringVar(
	&projectsExpectedRepository,
	"expected-repository",
	"",
	"Require the exact credential-free repository identity",
)
_ = projectsRemoveCmd.MarkFlagRequired("expected-repository")
projectsRemoveCmd.RunE = withGracefulSignals(runProjectsRemove)
```

Replace `unregisterProject` with an injectable `removeProjectThroughDaemon`, capture expansion once, and preserve the existing success envelope:

```go
func runProjectsRemove(cmd *cobra.Command, args []string) error {
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return writeProjectServiceError(cmd, service.NewError(
			service.UnregistrationFailed, "failed to capture project removal context", false, nil, err,
		))
	}
	result, err := removeProjectThroughDaemon(cmd.Context(), kwt.ProjectRemovalRequest{
		Path: args[0], ExpectedRepository: projectsExpectedRepository, Expansion: expansion,
	})
	if err != nil {
		return writeProjectServiceError(cmd, service.AsError(err))
	}
	return writeProjectMutationResult(cmd, projectsRemoveJSON, "unregistered", result.Project)
}
```

`removeProjectThroughDaemon` must call `requireDaemonCapability(CapabilityProjectRemoval)` and `Client.RemoveProject`; it may wait through a draining daemon using the existing drain-deadline policy, but it must never call configuration removal directly.

- [ ] **Step 4: Preserve exact machine-readable errors**

Refactor `writeProjectCommandError` so a `service.Descriptor` is encoded without translating its code or retryability. Preserve the existing projects JSON envelope byte shape from #72, including additive safe `details`; retain current non-JSON messages and exit-code mapping. Do not normalize exit codes outside this command.

- [ ] **Step 5: Run CLI and subprocess tests**

Run: `GOFLAGS=-buildvcs=false go test ./internal/cmd -run 'TestProjectsRemove|TestDaemonSubprocess.*Project' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the CLI migration**

```bash
git add internal/cmd/projects.go internal/cmd/projects_test.go internal/cmd/daemon_client.go internal/cmd/daemon_subprocess_test.go
git commit -m 'Route project unregistration through the daemon'
```

### Task 7: Fence registered worktree creation and pull-request import

**Files:**
- Create: `internal/cmd/project_guard.go`
- Modify: `internal/cmd/add.go:105-330`
- Modify: `internal/cmd/pr.go:133-237,563-635`
- Test: `internal/cmd/add_test.go`
- Test: `internal/cmd/pr_test.go`
- Test: `internal/lifecycle/project_fence_test.go`

- [ ] **Step 1: Write deterministic race tests with blocking hooks**

Do not rely on sleeps. Add hooks around fence acquisition and mutation, then prove both orderings for add, PR import, and protected attach:

```go
func TestRegisteredAddLosesToProjectRemoval(t *testing.T) {
	// Observe the registration in the add command, block before fence acquisition,
	// remove it under the fence, unblock add, and assert registration_changed.
	// Assert no worktree path or registry record was created.
}

func TestProjectRemovalObservesCompletedPRImport(t *testing.T) {
	// Let import acquire the fence, persist provenance and optionally establish
	// the protected session, then allow removal to acquire. Assert removal sees
	// the completed authority/live session rather than an intermediate state.
}

func TestProtectedAttachReleasesFenceBeforeBlockingClient(t *testing.T) {
	// Block the client attach after Ensure succeeds; prove another fence waiter
	// can acquire while the attach call remains blocked.
}
```

- [ ] **Step 2: Run the race tests and confirm operations are currently unfenced**

Run: `GOFLAGS=-buildvcs=false go test -race ./internal/cmd -run 'Test(RegisteredAdd|ProjectRemovalObservesCompletedPRImport|ProtectedAttachReleasesFence)' -count=1`

Expected: FAIL because creation/import/attach do not observe and revalidate a project claim.

- [ ] **Step 3: Add a command-level project guard**

Create:

```go
type guardedProjectOperation struct {
	home  string
	claim *lifecycle.ProjectClaim
}

func observeGuardedProjectOperation(
	ctx context.Context,
	home string,
	mainPath string,
	expansion kwt.ExpansionContext,
) (*guardedProjectOperation, error) {
	claim, err := lifecycle.ObserveProjectClaim(ctx, home, mainPath, expansion)
	return &guardedProjectOperation{home: home, claim: claim}, err
}

func (g *guardedProjectOperation) run(
	ctx context.Context, mutation func() error,
) (err error) {
	release, err := lifecycle.AcquireProjectClaim(ctx, g.home, g.claim)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return mutation()
}
```

`lifecycle.ObserveProjectClaim` loads the global snapshot through the expansion context inside the lifecycle package, where `ExpansionContext.expandPath` is available. It must keep repository-local config/trust loading outside the fence and revalidate the observed raw registration after acquisition.

- [ ] **Step 4: Wrap only the irreversible add/import section**

In `runAdd`, resolve config, main repository, arguments, layouts, and registration claim first; acquire the claim immediately before the first worktree mutation and hold it through registry/provenance persistence and optional protected-session creation. A nil claim allows a newly started add after unregistration to proceed unchanged.

In `runPRImport`, observe the selected registered project before waiting, acquire/revalidate before `service.Import`, and hold through the optional `startPRWorkspaceSession`. Return `registration_changed` through the command's stable JSON error envelope if revalidation loses.

- [ ] **Step 5: Split protected ensure from blocking attach**

Replace the combined attach callback with two callbacks:

```go
var (
	ensurePRWorkspaceSession = defaultStartPRWorkspaceSession
	attachExistingPRWorkspaceSession = func(
		ctx context.Context, workspace pullrequest.Workspace, socketName string,
	) error {
		command := tmux.NewTmuxCommandForSocket("", socketName)
		return command.AttachSessionWithoutEnvironment(ctx, workspace.SessionName)
	}
)
```

For `pr attach`: resolve config/trust and provenance before the fence; acquire and revalidate the project claim and provenance; ensure the protected session; release the fence; then invoke the blocking client attach. If removal wins first, fail `registration_changed` before session creation.

- [ ] **Step 6: Preserve PR JSON error contracts**

Teach `writePRError` to detect `*service.Error` and emit its descriptor without exposing its private cause. Keep existing pull-request codes and exit codes untouched for native PR failures; `registration_changed` uses operational exit 1 and the shared `code/message/retryable/details` fields.

- [ ] **Step 7: Run command races and focused package tests**

Run: `GOFLAGS=-buildvcs=false go test -race ./internal/cmd ./internal/lifecycle -run 'Test(RegisteredAdd|ProjectRemovalObservesCompletedPRImport|ProtectedAttachReleasesFence|ProjectClaim)' -count=1`

Expected: PASS.

- [ ] **Step 8: Commit the competing-operation fencing**

```bash
git add internal/cmd/project_guard.go internal/cmd/add.go internal/cmd/add_test.go internal/cmd/pr.go internal/cmd/pr_test.go internal/lifecycle/project_fence_test.go
git commit -m 'Fence project-owned worktree mutations'
```

### Task 8: End-to-end behavior, documentation, and delivery cleanup

**Files:**
- Modify: `docs/reference/cli.md:390-410`
- Modify: `docs/design/daemon.md`
- Modify: `docs/development/threat-model.md:65-95`
- Test: `internal/cmd/daemon_subprocess_test.go`
- Delete before push: `docs/superpowers/specs/2026-08-10-guarded-project-unregistration-design.md`
- Delete before push: `docs/superpowers/plans/2026-08-10-guarded-project-unregistration.md`

- [ ] **Step 1: Add the final subprocess acceptance matrix**

Using a real daemon subprocess and temporary KWT_HOME, cover:

```go
func TestDaemonSubprocessGuardedProjectRemoval(t *testing.T) {
	// 1. `projects --json` publishes a missing exact registration and stable identity.
	// 2. `projects remove <exact> --expected-repository <identity> --json` succeeds.
	// 3. `/repo` cannot remove `/repo `.
	// 4. mismatched identity produces registration_changed and preserves metadata.
	// 5. malformed provenance produces protected_endpoint_inventory_incomplete.
	// 6. credential canaries are absent from stdout, stderr, and daemon.log.
}
```

The test may fake the protected probe only at the service boundary; do not create real ordinary/default-server tmux sessions just to prove the service never issues a kill command. That non-mutation belongs in the injected service tests.

- [ ] **Step 2: Run the acceptance test and fix only owned behavior failures**

Run: `GOFLAGS=-buildvcs=false go test ./internal/cmd -run TestDaemonSubprocessGuardedProjectRemoval -count=1 -v`

Expected: PASS.

- [ ] **Step 3: Update user and daemon documentation**

Document:

```text
kwt projects remove <exact-registered-path> \
  --expected-repository <identity> --json
```

State that the path is the exact value returned by `projects --json`, the identity is credential-free, removal is daemon-owned and metadata-only, live protected sessions fail closed, ordinary tmux sessions are untouched, and long-running creation/import commands retain their existing foreground/streaming status output. Add `project.removal.v1`, schema 1.5, and all five project error codes to the daemon design reference. Extend the threat model with the project fence, owner-private provenance authority, fail-closed probe boundary, and the rule that protected endpoint details are the only tmux identity returned publicly.

- [ ] **Step 4: Format and run the complete verification suite**

Run:

```bash
gofmt -w kwt.go service/error.go internal/config/config.go internal/worktree/worktree.go internal/worktree/worktree_test.go internal/lifecycle/project_identity.go internal/lifecycle/project_fence.go internal/lifecycle/project_removal.go internal/lifecycle/source.go internal/lifecycle/inventory.go internal/lifecycle/source_test.go internal/lifecycle/project_fence_test.go internal/lifecycle/project_removal_test.go internal/tmux/probe.go internal/tmux/probe_test.go internal/daemon/types.go internal/daemon/runtime.go internal/daemon/host.go internal/daemon/server.go internal/daemon/client.go internal/daemon/server_test.go internal/daemon/client_test.go internal/daemon/runtime_test.go internal/daemon/host_test.go internal/cmd/projects.go internal/cmd/projects_test.go internal/cmd/daemon_client.go internal/cmd/daemon_subprocess_test.go internal/cmd/project_guard.go internal/cmd/add.go internal/cmd/add_test.go internal/cmd/pr.go internal/cmd/pr_test.go
GOFLAGS=-buildvcs=false make test
GOFLAGS=-buildvcs=false make build
GOFLAGS=-buildvcs=false go test -race ./internal/lifecycle ./internal/daemon ./internal/cmd
git diff --check
```

Expected: all commands PASS and `git diff --check` emits no output.

- [ ] **Step 5: Record kata evidence**

```bash
kata comment qeva --body "Implemented exact-path, expected-repository, CAS-bound daemon project removal with credential-safe CLI/HTTP coverage; full tests, build, and focused race suites pass." --json
kata comment g683 --body "Implemented daemon-owned protected-endpoint guard and shared project fence across removal, add/import, and protected attach; missing-checkout and race coverage pass." --json
```

Close each issue only after its complete behavioral suite passes.

- [ ] **Step 6: Remove local planning artifacts before the delivery commit**

```bash
git rm docs/superpowers/specs/2026-08-10-guarded-project-unregistration-design.md
git rm docs/superpowers/plans/2026-08-10-guarded-project-unregistration.md
```

Verify: `git ls-files 'docs/superpowers/specs/*' 'docs/superpowers/plans/*'` prints nothing from this branch.

- [ ] **Step 7: Commit documentation and acceptance coverage**

```bash
git add docs/reference/cli.md docs/design/daemon.md docs/development/threat-model.md internal/cmd/daemon_subprocess_test.go
git commit -m 'Document guarded project unregistration'
```

- [ ] **Step 8: Re-run verification on the exact committed tree**

Run:

```bash
GOFLAGS=-buildvcs=false make test
GOFLAGS=-buildvcs=false make build
GOFLAGS=-buildvcs=false go test -race ./internal/lifecycle ./internal/daemon ./internal/cmd
git status --short
```

Expected: all verification passes and the worktree is clean.

- [ ] **Step 9: Push and open the single rationale-first PR**

```bash
git push -u origin ghosthub-guarded-project-unregister
gh pr create --base main --head ghosthub-guarded-project-unregister \
  --title 'Guard project unregistration against live sessions' \
  --body 'Ghosthub must be able to unregister unavailable projects without trusting stale inventory or orphaning protected tmux sessions. This makes removal an exact identity-bound daemon transaction, shares a project lifecycle fence with endpoint-producing operations, and keeps missing registrations visible through the credential-free projects JSON contract.'
```
