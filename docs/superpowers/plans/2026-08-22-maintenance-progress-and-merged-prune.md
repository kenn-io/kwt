# Maintenance Progress and Merged Prune Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `kwt doctor` and `kwt prune` bounded progress output, and make merged pruning prompt only for dirty worktrees whose pull request is proven merged.

**Architecture:** A command-local progress reporter owns stderr rendering and receives phase/count events from maintenance logic. Merged-prune policy proves provider relevance before checking cleanliness, then the command maps dirty confirmed-merged candidates to explicit schema-v2 confirmation outcomes.

**Tech Stack:** Go, Cobra, `golang.org/x/term`, existing `internal/maintenance`, `internal/prunepolicy`, and Git command adapters.

## Global Constraints

- Keep command-line Git; do not introduce `go-git`.
- Keep Git 2.31 as the maintenance-command minimum.
- Progress writes only to stderr; JSON stdout keeps a single report document.
- `doctor --quiet` emits neither progress nor the human report.
- `--force` remains invalid with `--merged`.
- Ignored files count as dirty and require confirmation before forced removal.
- `head_advanced_after_pr` remains a hard stop.
- Increase prune report schema from 1 to 2; do not add a version-1 output mode.
- Invoke `kenn:using-go` before Go changes, `kenn:test-scope-discipline` before tests, and `kenn:commit` before every commit.
- Follow test-first development: observe each focused test fail before production changes.

---

## File map

- Create `internal/cmd/maintenance_progress.go`: terminal/non-terminal progress state machine.
- Create `internal/cmd/maintenance_progress_test.go`: deterministic writer, clock, and tick tests.
- Modify `internal/cmd/doctor.go`: doctor phase reporting and quiet suppression.
- Modify `internal/cmd/prune.go`: expired-prune phase/count reporting.
- Modify `internal/cmd/prune_merged.go`: provider-first evaluation, fresh dirtiness checks, prompts, and forced confirmed removal.
- Modify `internal/cmd/prune_merged_test.go`: command behavior and prompt/recheck tests.
- Modify `internal/prunepolicy/merged.go`: remove the early dirty short circuit and classify dirty only after merge proof.
- Modify `internal/prunepolicy/merged_test.go`: provider-call and unrelated-dirty policy tests.
- Modify `internal/prunepolicy/types.go`: schema v2 reasons, summaries, and exit allowlist.
- Modify `internal/prunepolicy/types_test.go`: reason-driven summary and exit tests.
- Modify `internal/cmd/doctor_test.go` and `internal/cmd/prune_test.go`: progress integration tests.
- Modify `docs/reference/cli.md`: human and JSON contracts.

### Task 1: Build the maintenance progress reporter

**Files:**
- Create: `internal/cmd/maintenance_progress.go`
- Create: `internal/cmd/maintenance_progress_test.go`

**Interfaces:**
- Produces: `maintenanceProgress` with `Phase(string, int)`, `Set(int)`, `Pause()`, `Resume()`, and `Close()`.
- Produces: `newMaintenanceProgress(cmd *cobra.Command, enabled bool) maintenanceProgress`.
- Consumes: `cmd.ErrOrStderr()` and a package seam `maintenanceProgressIsTerminal(io.Writer) bool`.

- [ ] **Step 1: Write deterministic failing reporter tests**

Add table tests that use a fake clock and a caller-owned tick channel:

```go
func TestMaintenanceProgressTerminalReplacesOneLineAndUsesPerPhaseETA(t *testing.T) {
	var output bytes.Buffer
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time, 4)
	p := newMaintenanceProgressWithOptions(maintenanceProgressOptions{
		Writer: &output, Terminal: true,
		Now: func() time.Time { return now }, Ticks: ticks,
	})

	p.Phase("verify pull requests", 4)
	now = now.Add(2 * time.Second)
	p.Set(1)
	ticks <- now
	p.Phase("remove worktrees", 2)
	now = now.Add(time.Second)
	p.Set(1)
	p.Close()

	text := output.String()
	assert.Contains(t, text, "verify pull requests 1/4 · ETA 6s")
	assert.Contains(t, text, "remove worktrees 1/2 · ETA 1s")
	assert.NotContains(t, text, "ETA 3s", "ETA must reset at the phase boundary")
	assert.True(t, strings.HasSuffix(text, "\r\x1b[2K"))
}

func TestMaintenanceProgressNonTerminalEmitsOnlyQuartileMilestones(t *testing.T) {
	var output bytes.Buffer
	p := newMaintenanceProgressWithOptions(maintenanceProgressOptions{
		Writer: &output, Now: time.Now,
	})
	p.Phase("inspect worktrees", 100)
	for completed := 1; completed <= 100; completed++ {
		p.Set(completed)
	}
	p.Close()

	assert.Equal(t, strings.Join([]string{
		"kwt: inspect worktrees",
		"kwt: inspect worktrees 25/100",
		"kwt: inspect worktrees 50/100",
		"kwt: inspect worktrees 75/100",
		"kwt: inspect worktrees 100/100",
		"",
	}, "\n"), output.String())
}

func TestMaintenanceProgressPauseClearsBeforePromptAndResumeRedraws(t *testing.T) {
	var output bytes.Buffer
	p := newMaintenanceProgressWithOptions(maintenanceProgressOptions{
		Writer: &output, Terminal: true, Now: time.Now,
	})
	p.Phase("remove worktrees", 2)
	p.Set(1)
	p.Pause()
	_, _ = io.WriteString(&output, "Remove /worktree and all local files? [y/N] ")
	p.Resume()
	p.Close()

	assert.Contains(t, output.String(), "\r\x1b[2KRemove /worktree")
}
```

- [ ] **Step 2: Run the reporter tests and confirm the missing implementation failure**

Run: `go test ./internal/cmd -run '^TestMaintenanceProgress' -count=1`

Expected: FAIL because `newMaintenanceProgressWithOptions` and its types do not exist.

- [ ] **Step 3: Implement the reporter state machine**

Create these exact public-to-package interfaces and keep all rendering private:

```go
type maintenanceProgress interface {
	Phase(name string, total int)
	Set(completed int)
	Pause()
	Resume()
	Close()
}

type maintenanceProgressOptions struct {
	Writer   io.Writer
	Terminal bool
	Now      func() time.Time
	Ticks    <-chan time.Time
}

type maintenanceProgressState struct {
	writer        io.Writer
	terminal      bool
	now           func() time.Time
	phase         string
	total         int
	completed     int
	phaseStarted  time.Time
	lastMilestone int
	frame         int
	paused        bool
	closed        bool
	mu            sync.Mutex
}

func newMaintenanceProgress(cmd *cobra.Command, enabled bool) maintenanceProgress {
	if !enabled {
		return noopMaintenanceProgress{}
	}
	writer := cmd.ErrOrStderr()
	return newMaintenanceProgressWithOptions(maintenanceProgressOptions{
		Writer: writer, Terminal: maintenanceProgressIsTerminal(writer), Now: time.Now,
	})
}
```

`newMaintenanceProgressWithOptions` starts a 100 ms ticker only when `Terminal`
is true and `Ticks` is nil. On each tick it locks the state, advances the frame
through `|/-\\`, and redraws `\r\x1b[2K<spinner> <phase>`. `Set` clamps to
`[0,total]`; terminal mode redraws immediately, while non-terminal mode writes
only when `completed*4/total` advances to 1, 2, 3, or 4. ETA is
`elapsed/completed*(total-completed)`, rounded to seconds. `Pause` clears the
terminal line, `Resume` redraws it, and `Close` stops the owned ticker and clears
the line exactly once. Implement every method as a no-op on
`noopMaintenanceProgress`.

The ticker goroutine selects on both the tick channel and an owned stop channel.
`Close` closes the stop channel and waits on an owned `done` channel before it
returns, so no goroutine or writer use can outlive the command.

Use this exact terminal seam:

```go
var maintenanceProgressIsTerminal = func(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}
```

- [ ] **Step 4: Run focused reporter tests**

Run: `go test ./internal/cmd -run '^TestMaintenanceProgress' -race -count=1`

Expected: PASS with no race report.

- [ ] **Step 5: Commit the reporter**

Run `kenn:commit`, then commit only the two reporter files with subject:

```text
Show bounded maintenance progress
```

### Task 2: Define schema-v2 confirmation outcomes

**Files:**
- Modify: `internal/prunepolicy/types.go`
- Create: `internal/prunepolicy/types_test.go`

**Interfaces:**
- Produces: reasons `WouldRequireConfirmation`, `ConfirmationRequired`, and `ConfirmationDeclined`.
- Produces: `SchemaVersion == 2` and reason-driven `Report.Finalize`/`Report.ExitCode` behavior.

- [ ] **Step 1: Write failing summary and exit tests**

```go
func TestConfirmationReasonsHaveDistinctSummaryAndExitSemantics(t *testing.T) {
	tests := []struct {
		reason      Reason
		wouldRemove int
		skipped     int
		exitCode    int
	}{
		{WouldRequireConfirmation, 1, 0, 0},
		{ConfirmationDeclined, 0, 1, 0},
		{ConfirmationRequired, 0, 1, 1},
	}
	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			report := Report{Outcomes: []Outcome{{Reason: tt.reason}}}
			report.Finalize()
			assert.Equal(t, tt.wouldRemove, report.Summary.WouldRemove)
			assert.Equal(t, tt.skipped, report.Summary.Skipped)
			assert.Equal(t, tt.exitCode, report.ExitCode())
		})
	}
	assert.Equal(t, 2, SchemaVersion)
}
```

- [ ] **Step 2: Run the policy test and confirm undefined reasons**

Run: `go test ./internal/prunepolicy -run '^TestConfirmationReasons' -count=1`

Expected: FAIL because the three reason constants do not exist.

- [ ] **Step 3: Add schema-v2 reasons and allowlists**

Add:

```go
const SchemaVersion = 2

const (
	WouldRequireConfirmation Reason = "would_require_confirmation"
	ConfirmationRequired     Reason = "confirmation_required"
	ConfirmationDeclined     Reason = "confirmation_declined"
)
```

In `Finalize`, count `WouldRequireConfirmation` with `WouldRemove`. In
`ExitCode`, allow `WouldRequireConfirmation` and `ConfirmationDeclined` beside
the existing success reasons. Do not allow `ConfirmationRequired`.

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/prunepolicy -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the schema contract**

Run `kenn:commit`, then commit `types.go` and `types_test.go` with subject:

```text
Classify merged-prune confirmation outcomes
```

### Task 3: Prove merge relevance before dirtiness

**Files:**
- Modify: `internal/prunepolicy/merged.go`
- Modify: `internal/prunepolicy/merged_test.go`

**Interfaces:**
- Consumes: schema-v2 reason constants from Task 2.
- Produces: provider evaluation that returns `EligibleMerged` for both clean and dirty confirmed-merged candidates; the command layer performs the final fresh dirtiness classification.

- [ ] **Step 1: Add failing provider-order tests**

```go
func TestEvaluateMergedIgnoresDirtyStateUntilAfterProviderProof(t *testing.T) {
	provider := providerEmpty()
	candidate := dirtyMerged()

	outcome := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})[0]

	assert.Equal(t, NoAssociatedPR, outcome.Reason)
	assert.NotZero(t, provider.resolveCalls)
	assert.NotZero(t, provider.commitCalls)
}

func TestEvaluateMergedReturnsEligibleForDirtyConfirmedMergedCandidate(t *testing.T) {
	provider := providerForCommit(exactMergedPR(forkRepository))
	candidate := dirtyMerged()

	outcome := EvaluateMerged(context.Background(), provider, []MergedCandidate{candidate})[0]

	assert.Equal(t, EligibleMerged, outcome.Reason)
}

func TestEvaluateMergedKeepsAdvancedDirtyHeadAsHardStop(t *testing.T) {
	candidate := advancedCandidate()
	candidate.Dirty = true
	outcome := EvaluateMerged(context.Background(), providerForAdvancedBranch(), []MergedCandidate{candidate})[0]
	assert.Equal(t, HeadAdvancedAfterPR, outcome.Reason)
}
```

- [ ] **Step 2: Run the new policy tests and observe the dirty short circuit**

Run: `go test ./internal/prunepolicy -run 'DirtyState|DirtyConfirmed|AdvancedDirty' -count=1`

Expected: FAIL with `dirty_worktree` returned before provider calls.

- [ ] **Step 3: Remove policy-layer dirty classification**

Delete the `case candidate.Dirty` arm from the initial switch in
`evaluateMergedCandidate`. Do not add a later dirty arm: after structural,
provider, exact-head, and merged-state checks succeed, return the existing
`EligibleMerged` result. Retain `Dirty` temporarily on `MergedCandidate` until
Task 4 removes collection-time dependence; this keeps the change narrow.

- [ ] **Step 4: Run all merged-policy tests**

Run: `go test ./internal/prunepolicy -run 'TestEvaluateMerged' -count=1`

Expected: PASS after updating the old `dirty` table case to expect
`EligibleMerged`.

- [ ] **Step 5: Commit provider-first classification**

Run `kenn:commit`, then commit both merged-policy files with subject:

```text
Prove merged PRs before checking dirtiness
```

### Task 4: Add fresh dirty confirmation and evidence-bound force

**Files:**
- Modify: `internal/cmd/prune_merged.go`
- Modify: `internal/cmd/prune_merged_test.go`
- Modify: `internal/cmd/prune_test.go`
- Modify: `internal/cmd/prune.go`

**Interfaces:**
- Consumes: `EligibleMerged` and the three schema-v2 reasons.
- Consumes: `newMaintenanceProgress` from Task 1.
- Produces: `inspectPruneMergedDirty(candidate) (bool, error)` and `confirmPruneMergedDirty(cmd, candidate) (bool, error)` seams.
- Changes: `removePruneMergedCandidate(..., force bool)` and `defaultRemovePruneMergedWorktree(candidate, force, claim)`.

- [ ] **Step 1: Write failing command tests for all confirmation modes**

Add tests with injected dirtiness and prompt seams:

```go
func TestPruneMergedDirtyDryRunWouldRequireConfirmation(t *testing.T) {
	resetPruneMergedCommand(t)
	pruneDryRun = true
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/topic", commandMergedHead)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	inspectPruneMergedDirty = func(pruneMergedCandidate) (bool, error) { return true, nil }
	cmd, stdout, _ := fleetTestCommand()

	err := runPruneMerged(cmd, nil)

	require.NoError(t, err)
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, prunepolicy.WouldRequireConfirmation, report.Outcomes[0].Reason)
	assert.Equal(t, 1, report.Summary.WouldRemove)
}

func TestPruneMergedDirtyNonInteractiveRequiresConfirmation(t *testing.T) {
	resetPruneMergedCommand(t)
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/topic", commandMergedHead)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	inspectPruneMergedDirty = func(pruneMergedCandidate) (bool, error) { return true, nil }
	cmd, stdout, _ := fleetTestCommand()

	err := runPruneMerged(cmd, nil)

	assertExitCode(t, err, 1)
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, prunepolicy.ConfirmationRequired, report.Outcomes[0].Reason)
}

func TestPruneMergedDirtyPromptRechecksAndForcesOnlyAfterYes(t *testing.T) {
	resetPruneMergedCommand(t)
	candidate := commandMergedCandidate("/worktrees/topic", commandMergedHead)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	var inspections int
	inspectPruneMergedDirty = func(pruneMergedCandidate) (bool, error) {
		inspections++
		return true, nil
	}
	confirmPruneMergedDirty = func(_ *cobra.Command, candidate pruneMergedCandidate) (bool, error) {
		assert.Contains(t, candidate.Policy.Path, "worktree")
		return true, nil
	}
	var forced bool
	removePruneMergedWorktree = func(_ pruneMergedCandidate, force bool, claim func(func() error) (bool, error)) (bool, error) {
		forced = force
		return true, nil
	}
	cmd, _, _ := fleetTestCommand()

	require.NoError(t, runPruneMerged(cmd, nil))
	assert.Equal(t, 2, inspections, "classify after proof, then refresh before prompt")
	assert.True(t, forced)
}

func TestPruneMergedIgnoredOnlyWorktreePromptsForCompleteDirectory(t *testing.T) {
	repo := newTUITestRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("build/\n"), 0o644))
	runTUITestGit(t, repo, "add", ".gitignore")
	runTUITestGit(t, repo, "commit", "-m", "ignore build output")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "build", "output"), []byte("artifact"), 0o644))
	dirty, err := defaultInspectPruneMergedDirty(pruneMergedCandidate{
		RepositoryRoot: repo, Policy: prunepolicy.MergedCandidate{Path: repo},
	})
	require.NoError(t, err)
	assert.True(t, dirty)
}
```

Also add a declined-prompt test expecting `ConfirmationDeclined`, exit 0, and
no removal call; a prompt-time-clean test expecting non-forced removal; and a
prompt inspection error test expecting `PathUnavailable`.

Add one human-rendering regression:

```go
func TestRenderPruneReportGroupsRepeatedOutcomes(t *testing.T) {
	cmd, stdout, _ := fleetTestCommand()
	report := prunepolicy.Report{Policy: "merged", Outcomes: []prunepolicy.Outcome{
		{Path: "/work/one", Reason: prunepolicy.ConfirmationRequired, Message: "confirmation required", Remediation: "Run interactively."},
		{Path: "/work/two", Reason: prunepolicy.ConfirmationRequired, Message: "confirmation required", Remediation: "Run interactively."},
	}}
	report.Finalize()
	require.NoError(t, renderPruneReport(cmd, report, false))
	text := stdout.String()
	assert.Equal(t, 1, strings.Count(text, "[confirmation_required]"))
	assert.Contains(t, text, "/work/one")
	assert.Contains(t, text, "/work/two")
	assert.Equal(t, 1, strings.Count(text, "Run interactively."))
}
```

- [ ] **Step 2: Run focused command tests and observe missing seams/reasons**

Run: `go test ./internal/cmd -run 'TestPruneMerged.*(Confirmation|Prompt|Ignored)|TestRenderPruneReportGroups' -count=1`

Expected: FAIL because final dirtiness is not inspected after provider proof and
removal has no force argument.

- [ ] **Step 3: Implement fresh classification and prompt handling**

Add package seams:

```go
var inspectPruneMergedDirty = defaultInspectPruneMergedDirty
var confirmPruneMergedDirty = defaultConfirmPruneMergedDirty

func defaultInspectPruneMergedDirty(candidate pruneMergedCandidate) (bool, error) {
	output, err := git.New(candidate.Policy.Path).RunCommandWithoutCredentials(
		candidate.ProtectedNames,
		"status", "--ignored", "--porcelain", "--untracked-files=normal",
	)
	return strings.TrimSpace(output) != "", err
}
```

Stop setting `candidate.Policy.Dirty` in candidate discovery. After an outcome
is `EligibleMerged`, call `inspectPruneMergedDirty`. Map an inspection failure
through `pruneMergedOutcomeForError`. For dirty candidates:

```go
switch {
case pruneDryRun:
	outcome.Reason = prunepolicy.WouldRequireConfirmation
	outcome.Message = "merged worktree has local files or changes and would require confirmation"
case pruneJSON || !stdinIsTerminal():
	outcome.Reason = prunepolicy.ConfirmationRequired
	outcome.Message = "merged worktree has local files or changes and requires interactive confirmation"
default:
	progress.Pause()
	dirty, err = inspectPruneMergedDirty(candidate)
	if err == nil && dirty {
		approved, err = confirmPruneMergedDirty(cmd, candidate)
	}
	progress.Resume()
}
```

Construct `progress := newMaintenanceProgress(cmd, true)` at the start of
`runPruneMerged`, defer `progress.Close()`, and pause/resume that same reporter
around the prompt. Task 5 adds the complete phase/count wiring.

The default prompt writes this exact meaning to stderr and reads one line from
stdin: `Pull request merged. Remove <path> and all local changes and files in it, including ignored files and files added before removal? [y/N]`.
Only case-insensitive `y` or `yes` approves.

Thread `force bool` through `removePruneMergedCandidate` and
`removePruneMergedWorktree`. Pass it to
`RemoveWorktreeCheckedAfterClaim`; keep every existing removal condition and
ownership guard unchanged.

Update `fakePruneMergedRemoval` to accept the new force argument, and update
`resetPruneMergedDeps` to save and restore both `inspectPruneMergedDirty` and
`confirmPruneMergedDirty` so tests cannot leak prompt behavior into one another.

Refactor only the human branch of `renderPruneReport`. Group outcomes by the
tuple `(Reason, Message, Remediation)`, sort groups by reason and their paths by
`utils.PathKey`, print the reason/message once, list every path beneath it, and
print remediation once. Keep JSON rendering and the final summary unchanged.

- [ ] **Step 4: Run policy and command suites**

Run: `go test ./internal/prunepolicy ./internal/cmd -run 'PruneMerged|ConfirmationReasons|RenderPruneReportGroups' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit dirty confirmation**

Run `kenn:commit`, then commit the touched prune files with subject:

```text
Confirm removal of dirty merged worktrees
```

### Task 5: Wire progress into doctor and both prune policies

**Files:**
- Modify: `internal/cmd/doctor.go`
- Modify: `internal/cmd/doctor_test.go`
- Modify: `internal/cmd/prune.go`
- Modify: `internal/cmd/prune_test.go`
- Modify: `internal/cmd/prune_merged.go`
- Modify: `internal/cmd/prune_merged_test.go`

**Interfaces:**
- Consumes: `newMaintenanceProgress` from Task 1.
- Produces: phase updates around existing inspection/fix/evaluation/removal boundaries.

- [ ] **Step 1: Add failing command-level progress tests**

Inject `maintenanceProgressIsTerminal = func(io.Writer) bool { return false }`
and assert these bounded milestones:

```go
func TestDoctorReportsInspectionAndVerificationProgress(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorFix = true
	var inspections int
	doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
		inspections++
		if inspections == 1 { return doctorFindingReport(), nil }
		return doctorHealthyReport(), nil
	}
	doctorApplyFixes = func(context.Context, maintenance.Report, doctorRegistry, []*registry.WorktreeEntry) error { return nil }
	cmd, _, stderr := doctorTestCommand()

	require.NoError(t, runDoctor(cmd, nil))
	text := stderr.String()
	assert.Contains(t, text, "kwt: inspect worktrees")
	assert.Contains(t, text, "kwt: apply fixes 1/1")
	assert.Contains(t, text, "kwt: verify repairs")
}

func TestDoctorQuietSuppressesProgress(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorQuiet = true
	doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
		return doctorHealthyReport(), nil
	}
	cmd, stdout, stderr := doctorTestCommand()
	require.NoError(t, runDoctor(cmd, nil))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
}

func TestPruneMergedJSONKeepsProgressOffStdout(t *testing.T) {
	resetPruneMergedCommand(t)
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/topic", commandMergedHead)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	inspectPruneMergedDirty = func(pruneMergedCandidate) (bool, error) { return false, nil }
	cmd, stdout, stderr := fleetTestCommand()
	_ = runPruneMerged(cmd, nil)
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.Contains(t, stderr.String(), "kwt: verify pull requests")
}
```

- [ ] **Step 2: Run focused progress integration tests**

Run: `go test ./internal/cmd -run 'Progress|QuietSuppresses' -count=1`

Expected: FAIL because the commands do not construct or update a reporter.

- [ ] **Step 3: Add phase/count calls without changing business outcomes**

At the top of each run function:

```go
progress := newMaintenanceProgress(cmd, !doctorQuiet) // doctor
defer progress.Close()
```

Use `true` for prune. Set totals at boundaries already known to the command:

- doctor: load inventory (`0` unknown), inspect worktrees (`0` unknown), apply
  fixes (`report.Summary.FixableFindings`), verify repairs (`0` unknown);
- expired prune: load candidates, validate candidates (`len(expired)`), remove
  candidates (`len(expired)`);
- merged prune: discover candidates, verify pull requests (`len(policyCandidates)`),
  remove candidates (`count EligibleMerged`).

Call `Set` once after each processed item. Evaluate merged policy candidates one
at a time through the existing sequential provider so each result advances the
counter; retain the lazy provider instance across the loop. Because doctor
inspector calls are coarse and their true repository total is discovered inside
the call, leave those phases spinner-only rather than displaying a false total.
Complete the known fix total at the fix call boundary. Do not add callbacks to
`internal/maintenance` merely to animate internal loops. Pause before final
report rendering and every prompt.

- [ ] **Step 4: Run command packages with race detection**

Run: `go test ./internal/cmd -race -count=1`

Expected: PASS with no race report.

- [ ] **Step 5: Commit command progress integration**

Run `kenn:commit`, then commit the command files with subject:

```text
Explain doctor and prune progress
```

### Task 6: Document and verify the maintenance contract

**Files:**
- Modify: `docs/reference/cli.md`

**Interfaces:**
- Documents: progress streams, schema v2, all confirmation reasons, ignored-file deletion, and non-interactive behavior.

- [ ] **Step 1: Update maintained CLI documentation**

Add a `Merged worktree confirmation` subsection under `kwt prune` with this
contract:

```text
KWT proves that the associated pull request merged before it checks local
files. An interactive run asks before removing a confirmed merged worktree
that contains tracked, untracked, or ignored files. Approval removes the
complete worktree directory. JSON and redirected-input runs never prompt.

Prune JSON uses schema_version 2. Dirty merged dry runs report
would_require_confirmation, unattended runs report confirmation_required,
and declined prompts report confirmation_declined.
```

Document that progress goes to stderr, TTY output uses one updating line,
redirected output uses bounded milestones, JSON stdout remains one document,
and `doctor --quiet` suppresses progress.

- [ ] **Step 2: Run formatting and focused verification**

Run: `make fmt && go test ./internal/prunepolicy ./internal/cmd -count=1 && make docs-check`

Expected: all commands PASS.

- [ ] **Step 3: Review the complete plan-one diff**

Run: `git diff --check && git diff --stat && git status --short`

Expected: only plan-one implementation and maintained CLI documentation are
modified; `git diff --check` prints nothing.

- [ ] **Step 4: Commit documentation**

Run `kenn:commit`, then commit `docs/reference/cli.md` with subject:

```text
Document maintenance progress and confirmation
```

- [ ] **Step 5: Record completion evidence without closing the parent issue**

Run:

```text
kata comment wbcw --body "Landed maintenance progress and schema-v2 merged-prune confirmation. Verified internal/prunepolicy, internal/cmd, formatting, and docs-check." --agent
```

The parent issue stays open because scoped TUI refresh and the removal queue
remain.
