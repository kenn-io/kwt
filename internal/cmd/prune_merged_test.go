package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/prunepolicy"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

const (
	commandMergedHead = "0123456789abcdef0123456789abcdef01234567"
	commandBaseRepo   = "github.com/acme/widget"
	commandSourceRepo = "github.com/octocat/widget"
)

type fakePruneMergedRegistry struct {
	removed       bool
	err           error
	calls         int
	entries       []*registry.WorktreeEntry
	creationBusy  bool
	creationCalls int
	releaseErr    error
	entryMatches  func(string, *registry.WorktreeEntry) (bool, error)
}

type observedPruneMergedRegistry struct {
	*registry.Registry
	checked chan struct{}
	once    sync.Once
}

func (r *observedPruneMergedRegistry) EntryMatches(
	path string,
	expected *registry.WorktreeEntry,
) (bool, error) {
	matched, err := r.Registry.EntryMatches(path, expected)
	if err == nil && matched {
		r.once.Do(func() { close(r.checked) })
	}
	return matched, err
}

func (r *fakePruneMergedRegistry) List() []*registry.WorktreeEntry { return r.entries }

func (r *fakePruneMergedRegistry) RemoveIfMatchAfter(
	_ string,
	_ *registry.WorktreeEntry,
	removal func() error,
) (bool, error) {
	r.calls++
	if r.err != nil || !r.removed {
		return false, r.err
	}
	if err := removal(); err != nil {
		return false, err
	}
	return true, nil
}

func (r *fakePruneMergedRegistry) AcquireCreation(string) (func() error, bool, error) {
	r.creationCalls++
	if r.creationBusy {
		return nil, false, nil
	}
	return func() error { return r.releaseErr }, true, nil
}

func (r *fakePruneMergedRegistry) EntryMatches(
	path string,
	expected *registry.WorktreeEntry,
) (bool, error) {
	if r.entryMatches == nil {
		return true, nil
	}
	return r.entryMatches(path, expected)
}

func fakePruneMergedRemoval(
	remove func(pruneMergedCandidate) error,
) func(pruneMergedCandidate, func(func() error) (bool, error)) (bool, error) {
	return func(
		candidate pruneMergedCandidate,
		claim func(func() error) (bool, error),
	) (bool, error) {
		return claim(func() error { return remove(candidate) })
	}
}

type fakePruneMergedProvenance struct {
	records       map[string]pullrequest.Provenance
	viewRecords   func(int) map[string]pullrequest.Provenance
	viewCalls     int
	removeResult  bool
	removeErr     error
	removedKey    string
	removedRecord pullrequest.Provenance
}

func (s *fakePruneMergedProvenance) View(
	_ context.Context, fn func(map[string]pullrequest.Provenance) error,
) error {
	s.viewCalls++
	current := s.records
	if s.viewRecords != nil {
		current = s.viewRecords(s.viewCalls)
	}
	records := make(map[string]pullrequest.Provenance, len(current))
	for key, value := range current {
		records[key] = value
	}
	return fn(records)
}

func (s *fakePruneMergedProvenance) RemoveIfMatch(
	_ context.Context, key string, expected pullrequest.Provenance,
) (bool, error) {
	s.removedKey = key
	s.removedRecord = expected
	return s.removeResult, s.removeErr
}

type fakePruneMergedProvider struct {
	forCommit func(string) ([]pullrequest.PullRequest, error)
	byHead    func(string, string) ([]pullrequest.PullRequest, error)
	get       func(int) (pullrequest.PullRequest, error)
}

func (p *fakePruneMergedProvider) ResolveRepository(
	_ context.Context,
	repository pullrequest.Repository,
) (pullrequest.Repository, error) {
	return repository, nil
}

func (p *fakePruneMergedProvider) Get(
	_ context.Context, _ pullrequest.Repository, number int,
) (pullrequest.PullRequest, error) {
	if p.get == nil {
		return pullrequest.PullRequest{}, errors.New("unexpected Get")
	}
	return p.get(number)
}

func (p *fakePruneMergedProvider) ListForCommit(
	_ context.Context, _ pullrequest.Repository, sha string,
) ([]pullrequest.PullRequest, error) {
	if p.forCommit == nil {
		return nil, errors.New("unexpected ListForCommit")
	}
	return p.forCommit(sha)
}

func (p *fakePruneMergedProvider) ListByHead(
	_ context.Context, _ pullrequest.Repository, owner, branch string,
) ([]pullrequest.PullRequest, error) {
	if p.byHead == nil {
		return nil, nil
	}
	return p.byHead(owner, branch)
}

func TestPruneMergedDryRunAndJSONDoNotMutateOrPublish(t *testing.T) {
	resetPruneMergedCommand(t)
	pruneDryRun = true
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/topic", commandMergedHead)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	validated := 0
	validatePruneMergedWorktree = func(got pruneMergedCandidate) error {
		validated++
		assert.Equal(t, candidate, got)
		return nil
	}
	removed := 0
	removePruneMergedWorktree = fakePruneMergedRemoval(
		func(pruneMergedCandidate) error { removed++; return nil },
	)
	publications := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, stderr := fleetTestCommand()

	err := runPrune(cmd, nil)

	require.NoError(t, err, "stdout=%s stderr=%s", stdout.String(), stderr.String())
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, "merged", report.Policy)
	assert.True(t, report.DryRun)
	require.Len(t, report.Outcomes, 1)
	assert.Equal(t, prunepolicy.WouldRemove, report.Outcomes[0].Reason)
	assert.Equal(t, 1, validated)
	assert.Zero(t, removed)
	assert.Zero(t, publications)
}

func TestPruneMergedDryRunMapsRevalidationChanges(t *testing.T) {
	resetPruneMergedCommand(t)
	pruneDryRun = true
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/changed", commandMergedHead)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	validatePruneMergedWorktree = func(got pruneMergedCandidate) error {
		return &git.ConditionError{Reason: git.ReasonDirty, Path: got.Policy.Path}
	}
	removed := 0
	removePruneMergedWorktree = fakePruneMergedRemoval(
		func(pruneMergedCandidate) error { removed++; return nil },
	)
	cmd, stdout, stderr := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Len(t, report.Outcomes, 1)
	assert.Equal(t, prunepolicy.DirtyWorktree, report.Outcomes[0].Reason)
	assert.Equal(t, "https://github.com/acme/widget/pull/17", report.Outcomes[0].Evidence["pr_url"])
	assert.Empty(t, stderr.String())
	assert.Zero(t, removed)
}

func TestPruneMergedDoesNotRemoveWhileCandidateCreationIsActive(t *testing.T) {
	resetPruneMergedCommand(t)
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/creating", commandMergedHead)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	reg := &fakePruneMergedRegistry{creationBusy: true}
	openPruneMergedRegistry = func() (pruneMergedRegistry, error) { return reg, nil }
	removed := 0
	removePruneMergedWorktree = fakePruneMergedRemoval(func(pruneMergedCandidate) error {
		removed++
		return nil
	})
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), "creation")
	assert.Equal(t, 1, reg.creationCalls)
	assert.Zero(t, removed)
}

func TestPruneMergedDoesNotRemoveWhenRegistryOwnershipAppearsAfterSnapshot(
	t *testing.T,
) {
	resetPruneMergedCommand(t)
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/newly-registered", commandMergedHead)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	reg := &fakePruneMergedRegistry{
		entryMatches: func(string, *registry.WorktreeEntry) (bool, error) {
			return false, nil
		},
	}
	openPruneMergedRegistry = func() (pruneMergedRegistry, error) { return reg, nil }
	removed := 0
	removePruneMergedWorktree = fakePruneMergedRemoval(func(pruneMergedCandidate) error {
		removed++
		return nil
	})
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), "ownership")
	assert.Zero(t, removed)
}

func TestPruneMergedDoesNotRemoveWhenExpirationAppearsBeforeGitLock(t *testing.T) {
	resetPruneMergedCommand(t)
	t.Setenv("KWT_HOME", t.TempDir())
	pruneJSON = true
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/acme/widget.git")
	runTUITestGit(t, repositoryRoot, "remote", "add", "source", "https://github.com/octocat/widget.git")
	branch := "feature/concurrent-expiration"
	worktreePath := filepath.Join(t.TempDir(), "concurrent-expiration")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "config", "branch."+branch+".remote", "source")
	runTUITestGit(t, repositoryRoot, "config", "branch."+branch+".merge", "refs/heads/topic")
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	g := git.New(repositoryRoot)
	generation, err := g.WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	head := strings.TrimSpace(runTUITestGitOutput(t, worktreePath, "rev-parse", "HEAD"))
	candidate := commandMergedCandidate(worktreePath, head)
	candidate.RepositoryRoot = repositoryRoot
	candidate.Policy.Branch = branch
	candidate.Policy.Generation = generation
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	removePruneMergedWorktree = defaultRemovePruneMergedWorktree
	realRegistry, err := registry.New()
	require.NoError(t, err)
	registryChecked := make(chan struct{})
	observedRegistry := &observedPruneMergedRegistry{
		Registry: realRegistry,
		checked:  registryChecked,
	}
	openPruneMergedRegistry = func() (pruneMergedRegistry, error) {
		return observedRegistry, nil
	}
	gitLocked := make(chan struct{})
	expirationDone := make(chan error, 1)
	go func() {
		expirationDone <- g.WithWorktreeGeneration(worktreePath, generation, func() error {
			close(gitLocked)
			<-registryChecked
			expiresAt := time.Now().Add(time.Hour)
			updated, updateErr := realRegistry.SetExpirationIfGeneration(
				worktreePath,
				generation,
				commandBaseRepo,
				branch,
				&expiresAt,
			)
			if updateErr != nil {
				return updateErr
			}
			if !updated {
				return errors.New("expiration registration was not updated")
			}
			return nil
		})
	}()
	<-gitLocked
	cmd, stdout, _ := fleetTestCommand()

	err = runPrune(cmd, nil)

	require.NoError(t, <-expirationDone)
	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), "ownership")
	assert.DirExists(t, worktreePath)
	_, registered := realRegistry.Get(worktreePath)
	assert.True(t, registered)
}

func TestPruneMergedDoesNotRemoveWhenProvenanceAppearsAfterSnapshot(t *testing.T) {
	resetPruneMergedCommand(t)
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/newly-imported", commandMergedHead)
	record := commandMergedProvenance(candidate)
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	store := &fakePruneMergedProvenance{
		viewRecords: func(call int) map[string]pullrequest.Provenance {
			if call == 1 {
				return nil
			}
			return map[string]pullrequest.Provenance{record.PullRequestID: record}
		},
	}
	openPruneMergedProvenanceStore = func() pruneMergedProvenanceStore { return store }
	removed := 0
	removePruneMergedWorktree = fakePruneMergedRemoval(func(pruneMergedCandidate) error {
		removed++
		return nil
	})
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), "ownership")
	assert.Equal(t, 2, store.viewCalls)
	assert.Zero(t, removed)
}

func TestPruneMergedCleansOwnershipAfterRemovalWhenGuardReleaseFails(t *testing.T) {
	resetPruneMergedCommand(t)
	pruneJSON = true
	candidate := commandMergedCandidate("/worktrees/removed-before-unlock", commandMergedHead)
	candidate.RegistryExpected = true
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	reg := &fakePruneMergedRegistry{
		removed: true, releaseErr: errors.New("unlock failed"),
	}
	openPruneMergedRegistry = func() (pruneMergedRegistry, error) { return reg, nil }
	publications := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Len(t, report.Outcomes, 1)
	assert.Equal(t, prunepolicy.CleanupIncomplete, report.Outcomes[0].Reason)
	assert.Equal(t, "true", report.Outcomes[0].Evidence["worktree_removed"])
	assert.Contains(t, report.Outcomes[0].Evidence["cleanup_error"], "unlock failed")
	assert.Equal(t, 1, reg.calls)
	assert.Equal(t, 1, publications)
}

func TestPruneMergedRemovesWorktreePreservesBranchAndPublishesOnce(t *testing.T) {
	resetPruneMergedCommand(t)
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/acme/widget.git")
	runTUITestGit(t, repositoryRoot, "remote", "add", "source", "https://github.com/octocat/widget.git")
	branch := "feature/merged-real"
	worktreePath := filepath.Join(t.TempDir(), "merged-real")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "config", "branch."+branch+".remote", "source")
	runTUITestGit(t, repositoryRoot, "config", "branch."+branch+".merge", "refs/heads/topic")
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	generation, err := git.New(repositoryRoot).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	head := strings.TrimSpace(runTUITestGitOutput(t, worktreePath, "rev-parse", "HEAD"))
	candidate := commandMergedCandidate(worktreePath, head)
	candidate.RepositoryRoot = repositoryRoot
	candidate.Policy.Branch = branch
	candidate.Policy.Generation = generation
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	removePruneMergedWorktree = defaultRemovePruneMergedWorktree
	publications := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, stderr := fleetTestCommand()

	err = runPrune(cmd, nil)

	require.NoError(t, err, "stdout=%s stderr=%s", stdout.String(), stderr.String())
	assert.Contains(t, stdout.String(), string(prunepolicy.Removed))
	assert.NoDirExists(t, worktreePath)
	assert.Contains(t, runTUITestGitOutput(t, repositoryRoot, "branch", "--list", branch), branch)
	assert.Equal(t, 1, publications)
}

func TestPruneMergedRemovesMultipleGloballyDiscoveredWorktrees(t *testing.T) {
	resetPruneMergedCommand(t)
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/acme/widget.git")
	runTUITestGit(t, repositoryRoot, "remote", "add", "source", "https://github.com/octocat/widget.git")
	worktreeBase := t.TempDir()
	type worktreeFixture struct {
		path       string
		branch     string
		source     string
		head       string
		generation string
	}
	fixtures := []worktreeFixture{
		{path: filepath.Join(worktreeBase, "01-discovery"), branch: "feature/first", source: "first-topic"},
		{path: filepath.Join(worktreeBase, "02-other"), branch: "feature/second", source: "second-topic"},
	}
	entries := make([]*registry.WorktreeEntry, 0, len(fixtures))
	for index := range fixtures {
		fixture := &fixtures[index]
		runTUITestGit(t, repositoryRoot, "branch", fixture.branch)
		runTUITestGit(t, repositoryRoot, "config", "branch."+fixture.branch+".remote", "source")
		runTUITestGit(t, repositoryRoot, "config", "branch."+fixture.branch+".merge", "refs/heads/"+fixture.source)
		runTUITestGit(t, repositoryRoot, "worktree", "add", fixture.path, fixture.branch)
		runTUITestGit(t, fixture.path, "commit", "--allow-empty", "-m", fixture.branch)
		fixture.head = strings.TrimSpace(runTUITestGitOutput(t, fixture.path, "rev-parse", "HEAD"))
		var err error
		fixture.generation, err = git.New(repositoryRoot).WorktreeGeneration(fixture.path)
		require.NoError(t, err)
		entries = append(entries, &registry.WorktreeEntry{
			Path: fixture.path, Generation: fixture.generation, Repository: commandBaseRepo,
		})
	}
	loadPruneMergedConfig = func() (*models.Config, error) {
		return &models.Config{Worktree: models.WorktreeConfig{BaseDir: worktreeBase}}, nil
	}
	findPruneMergedGlobalPaths = func(string) ([]string, error) {
		return []string{fixtures[0].path, fixtures[1].path}, nil
	}
	inspectPruneMergedCandidates = defaultInspectPruneMergedCandidates
	openPruneMergedRegistry = func() (pruneMergedRegistry, error) {
		return &fakePruneMergedRegistry{removed: true, entries: entries}, nil
	}
	setPruneMergedProvider(&fakePruneMergedProvider{
		forCommit: func(sha string) ([]pullrequest.PullRequest, error) {
			for _, fixture := range fixtures {
				if fixture.head != sha {
					continue
				}
				candidate := commandMergedCandidate(fixture.path, fixture.head)
				candidate.Policy.Branch = fixture.branch
				candidate.Policy.SourceBranch = fixture.source
				return []pullrequest.PullRequest{commandMergedPR(candidate)}, nil
			}
			return nil, nil
		},
	})
	removePruneMergedWorktree = defaultRemovePruneMergedWorktree
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {}
	cmd, stdout, stderr := fleetTestCommand()
	cmd.SetContext(context.Background())

	err := runPrune(cmd, nil)

	require.NoError(t, err, "stdout=%s stderr=%s", stdout.String(), stderr.String())
	assert.NoDirExists(t, fixtures[0].path)
	assert.NoDirExists(t, fixtures[1].path)
}

func TestPruneMergedMapsRemovalPreconditionChanges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gitReason git.ConditionReason
		want      prunepolicy.Reason
	}{
		{name: "backlink", gitReason: git.ReasonBacklinkChanged, want: prunepolicy.DoctorRequired},
		{name: "generation", gitReason: git.ReasonGenerationChanged, want: prunepolicy.GenerationChanged},
		{name: "head", gitReason: git.ReasonHeadChanged, want: prunepolicy.HeadChanged},
		{name: "identity", gitReason: git.ReasonRepositoryChanged, want: prunepolicy.RepositoryChanged},
		{name: "branch", gitReason: git.ReasonBranchChanged, want: prunepolicy.SourceBranchMismatch},
		{name: "upstream repository", gitReason: git.ReasonUpstreamRepositoryChanged, want: prunepolicy.SourceRepositoryMismatch},
		{name: "upstream branch", gitReason: git.ReasonUpstreamBranchChanged, want: prunepolicy.SourceBranchMismatch},
		{name: "dirty", gitReason: git.ReasonDirty, want: prunepolicy.DirtyWorktree},
		{name: "locked", gitReason: git.ReasonLocked, want: prunepolicy.LockedWorktree},
		{name: "main", gitReason: git.ReasonMainWorktree, want: prunepolicy.MainWorktree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetPruneMergedCommand(t)
			pruneJSON = true
			candidate := commandMergedCandidate("/worktrees/"+tc.name, commandMergedHead)
			setPruneMergedInventory(candidate)
			setPruneMergedProvider(providerForCommandCandidates(candidate))
			removePruneMergedWorktree = fakePruneMergedRemoval(func(candidate pruneMergedCandidate) error {
				return &git.ConditionError{Reason: tc.gitReason, Path: candidate.Policy.Path}
			})
			publications := 0
			publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
			cmd, stdout, _ := fleetTestCommand()

			err := runPrune(cmd, nil)

			assertExitCode(t, err, 1)
			assert.Contains(t, stdout.String(), string(tc.want))
			var report prunepolicy.Report
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
			require.Len(t, report.Outcomes, 1)
			assert.Equal(t, "https://github.com/acme/widget/pull/17", report.Outcomes[0].Evidence["pr_url"])
			assert.Zero(t, publications)
		})
	}
}

func TestPruneMergedRemovalConditionsIncludeBranchAndUpstream(t *testing.T) {
	candidate := commandMergedCandidate("/worktrees/topic", commandMergedHead)
	candidate.ExpectedGitDir = "/projects/widget/.git/worktrees/topic"

	conditions := pruneMergedRemovalConditions(candidate)

	assert.Equal(t, candidate.ExpectedGitDir, conditions.ExpectedGitDir)
	assert.Equal(t, candidate.Policy.Branch, conditions.Branch)
	assert.Equal(t, candidate.Policy.SourceRepository, conditions.UpstreamRepository)
	assert.Equal(t, candidate.Policy.SourceBranch, conditions.UpstreamBranch)
	assert.True(t, conditions.IncludeIgnored)
}

func TestPruneMergedMissingGenerationDoesNotAuthenticateOrPublish(t *testing.T) {
	resetPruneMergedCommand(t)
	candidate := commandMergedCandidate("/worktrees/legacy", commandMergedHead)
	candidate.Policy.Generation = ""
	setPruneMergedInventory(candidate)
	providerCalls := 0
	newPruneMergedProvider = func(context.Context) (prunepolicy.MergedProvider, error) {
		providerCalls++
		return providerForCommandCandidates(candidate), nil
	}
	publications := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.MissingGeneration))
	assert.Zero(t, providerCalls)
	assert.Zero(t, publications)
}

func TestPruneMergedRemovesExactProvenance(t *testing.T) {
	resetPruneMergedCommand(t)
	candidate := commandMergedCandidate("/worktrees/imported", commandMergedHead)
	record := commandMergedProvenance(candidate)
	candidate.ProvenanceKey = record.PullRequestID
	candidate.Policy.Provenance = &record
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	store := &fakePruneMergedProvenance{records: map[string]pullrequest.Provenance{record.PullRequestID: record}, removeResult: true}
	openPruneMergedProvenanceStore = func() pruneMergedProvenanceStore { return store }
	cmd, _, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.Equal(t, record.PullRequestID, store.removedKey)
	assert.Equal(t, record, store.removedRecord)
}

func TestAttachMergedProvenanceIgnoresMismatchedGeneration(t *testing.T) {
	candidate := commandMergedCandidate("/worktrees/reused", "head-sha")
	record := commandMergedProvenance(candidate)
	record.Workspace.Generation = "fedcba9876543210fedcba9876543210"

	attachMergedProvenance(&candidate, map[string]pullrequest.Provenance{
		"stale": record,
	})

	assert.Nil(t, candidate.Policy.Provenance)
	assert.Empty(t, candidate.ProvenanceKey)
	assert.Nil(t, candidate.InitialOutcome)
}

func TestAttachMergedProvenanceAcceptsLegacyGeneration(t *testing.T) {
	candidate := commandMergedCandidate("/worktrees/legacy", "head-sha")
	record := commandMergedProvenance(candidate)
	record.Workspace.Generation = ""

	attachMergedProvenance(&candidate, map[string]pullrequest.Provenance{
		"legacy": record,
	})

	require.NotNil(t, candidate.Policy.Provenance)
	assert.Equal(t, record, *candidate.Policy.Provenance)
	assert.Equal(t, "legacy", candidate.ProvenanceKey)
}

func TestPruneMergedReportsCleanupIncompleteAfterRemoval(t *testing.T) {
	resetPruneMergedCommand(t)
	candidate := commandMergedCandidate("/worktrees/imported", commandMergedHead)
	record := commandMergedProvenance(candidate)
	candidate.ProvenanceKey = record.PullRequestID
	candidate.Policy.Provenance = &record
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	store := &fakePruneMergedProvenance{records: map[string]pullrequest.Provenance{record.PullRequestID: record}, removeResult: false}
	openPruneMergedProvenanceStore = func() pruneMergedProvenanceStore { return store }
	publications := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.CleanupIncomplete))
	assert.Equal(t, 1, publications)
}

type completedWorktreeRemovalWarning struct{ message string }

func (e completedWorktreeRemovalWarning) Error() string       { return e.message }
func (completedWorktreeRemovalWarning) WorktreeRemoved() bool { return true }

func TestPruneMergedCompletesBookkeepingAfterGitDeregistersWithResidualFiles(t *testing.T) {
	resetPruneMergedCommand(t)
	candidate := commandMergedCandidate("/worktrees/residual", commandMergedHead)
	record := commandMergedProvenance(candidate)
	candidate.ProvenanceKey = record.PullRequestID
	candidate.Policy.Provenance = &record
	candidate.RegistryExpected = true
	setPruneMergedInventory(candidate)
	setPruneMergedProvider(providerForCommandCandidates(candidate))
	reg := &fakePruneMergedRegistry{removed: true}
	openPruneMergedRegistry = func() (pruneMergedRegistry, error) { return reg, nil }
	store := &fakePruneMergedProvenance{
		records:      map[string]pullrequest.Provenance{record.PullRequestID: record},
		removeResult: true,
	}
	openPruneMergedProvenanceStore = func() pruneMergedProvenanceStore { return store }
	removePruneMergedWorktree = fakePruneMergedRemoval(func(pruneMergedCandidate) error {
		return completedWorktreeRemovalWarning{message: "worktree removed, but files remain"}
	})
	publications := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.CleanupIncomplete))
	assert.Contains(t, stdout.String(), "files remain")
	assert.Equal(t, 1, reg.calls)
	assert.Equal(t, record.PullRequestID, store.removedKey)
	assert.Equal(t, 1, publications)
}

func TestPruneMergedContinuesAfterPartialProviderFailure(t *testing.T) {
	resetPruneMergedCommand(t)
	failed := commandMergedCandidate("/worktrees/failed", commandMergedHead)
	confirmed := commandMergedCandidate("/worktrees/confirmed", "89abcdef0123456789abcdef0123456789abcdef")
	setPruneMergedInventory(failed, confirmed)
	provider := providerForCommandCandidates(failed, confirmed)
	provider.forCommit = func(sha string) ([]pullrequest.PullRequest, error) {
		if sha == failed.Policy.Head {
			return nil, pullrequest.NewError(pullrequest.CodeNetwork, "temporary", true, nil)
		}
		return []pullrequest.PullRequest{commandMergedPR(confirmed)}, nil
	}
	setPruneMergedProvider(provider)
	var removed []string
	removePruneMergedWorktree = fakePruneMergedRemoval(func(candidate pruneMergedCandidate) error {
		removed = append(removed, candidate.Policy.Path)
		return nil
	})
	publications := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.NetworkFailure))
	assert.Contains(t, stdout.String(), string(prunepolicy.Removed))
	assert.Equal(t, []string{confirmed.Policy.Path}, removed)
	assert.Equal(t, 1, publications)
}

func TestPruneMergedNoopDoesNotAuthenticateOrPublish(t *testing.T) {
	resetPruneMergedCommand(t)
	setPruneMergedInventory()
	providerCalls := 0
	newPruneMergedProvider = func(context.Context) (prunepolicy.MergedProvider, error) {
		providerCalls++
		return nil, errors.New("must not authenticate")
	}
	publications := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "No merged worktrees found")
	assert.Zero(t, providerCalls)
	assert.Zero(t, publications)
}

func TestPruneMergedInventoryFailureIsExecutionError(t *testing.T) {
	resetPruneMergedCommand(t)
	inspectPruneMergedCandidates = func(context.Context, *models.Config, map[string]pullrequest.Provenance, []*registry.WorktreeEntry) ([]pruneMergedCandidate, error) {
		return nil, errors.New("inventory unavailable")
	}
	cmd, _, stderr := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 2)
	assert.Contains(t, stderr.String(), "inventory unavailable")
}

func TestPruneMergedInventoryReturnsStrictGlobalDiscoveryError(t *testing.T) {
	resetPruneMergedDeps(t)
	wantErr := errors.New("global inventory incomplete")
	findPruneMergedGlobalPaths = func(string) ([]string, error) {
		return nil, wantErr
	}
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: t.TempDir()}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	assert.Nil(t, candidates)
	assert.ErrorIs(t, err, wantErr)
}

func TestPruneMergedInventoryReportsUninspectableDiscoveredPathAsDoctorRequired(t *testing.T) {
	resetPruneMergedDeps(t)
	badPath := filepath.Join(t.TempDir(), "stale-worktree")
	require.NoError(t, os.MkdirAll(badPath, 0o755))
	findPruneMergedGlobalPaths = func(string) ([]string, error) {
		return []string{badPath}, nil
	}
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: t.TempDir()}}

	candidates, err := defaultInspectPruneMergedCandidates(context.Background(), cfg, nil, nil)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.NotNil(t, candidates[0].InitialOutcome)
	assert.Equal(t, badPath, candidates[0].Policy.Path)
	assert.Equal(t, prunepolicy.DoctorRequired, candidates[0].InitialOutcome.Reason)
	assert.Contains(t, candidates[0].InitialOutcome.Message, "could not be inspected")
}

func TestPruneMergedInventoryIncludesFinalizedRegistryWorktreeRoot(t *testing.T) {
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/acme/widget.git")
	branch := "feature/registered-only"
	worktreePath := filepath.Join(t.TempDir(), "registered-only")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	generation, err := git.New(repositoryRoot).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	entry := &registry.WorktreeEntry{
		Path: worktreePath, Generation: generation, Repository: commandBaseRepo,
	}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), &models.Config{}, nil, []*registry.WorktreeEntry{entry},
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, utils.PathKey(worktreePath), utils.PathKey(candidates[0].Policy.Path))
	assert.Equal(t, commandBaseRepo, candidates[0].Policy.ProjectRepository)
	assert.True(t, candidates[0].RegistryExpected)
}

func TestPruneMergedInventoryRejectsRegistrySubdirectory(t *testing.T) {
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(
		t, repositoryRoot, "remote", "add", "origin",
		"https://github.com/acme/widget.git",
	)
	branch := "feature/registered-subdirectory"
	worktreePath := filepath.Join(t.TempDir(), "registered-subdirectory")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	_, err := git.New(repositoryRoot).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	registryPath := filepath.Join(worktreePath, "nested")
	require.NoError(t, os.Mkdir(registryPath, 0o755))
	entry := &registry.WorktreeEntry{
		Path: registryPath, Repository: commandBaseRepo,
	}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), &models.Config{}, nil, []*registry.WorktreeEntry{entry},
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, utils.PathKey(registryPath), utils.PathKey(candidates[0].Policy.Path))
	require.NotNil(t, candidates[0].InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, candidates[0].InitialOutcome.Reason)
	assert.Contains(t, candidates[0].InitialOutcome.Message, "exact worktree root")
}

func TestPruneMergedInventoryExcludesRegistryWorktreeWithCreationToken(t *testing.T) {
	entry := &registry.WorktreeEntry{
		Path: filepath.Join(t.TempDir(), "creating"), CreationToken: "creator",
	}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), &models.Config{}, nil, []*registry.WorktreeEntry{entry},
	)

	require.NoError(t, err)
	assert.Empty(t, candidates)
}

func TestPruneMergedRejectsMultipleRegistryAliasesForOneWorktree(t *testing.T) {
	realParent := t.TempDir()
	worktreePath := filepath.Join(realParent, "topic")
	require.NoError(t, os.Mkdir(worktreePath, 0o755))
	aliasParent := filepath.Join(t.TempDir(), "worktrees-link")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	generation := "0123456789abcdef0123456789abcdef"
	candidate := pruneMergedCandidate{Policy: prunepolicy.MergedCandidate{
		Path: worktreePath, Generation: generation,
		ProjectRepository: commandBaseRepo, LiveRepository: commandBaseRepo,
	}}
	entries := []*registry.WorktreeEntry{
		{Path: worktreePath, Generation: generation, Repository: commandBaseRepo},
		{Path: filepath.Join(aliasParent, "topic"), Generation: generation, Repository: commandBaseRepo},
	}

	attachMergedRegistrySnapshot(&candidate, entries)

	require.NotNil(t, candidate.InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, candidate.InitialOutcome.Reason)
	assert.Contains(t, candidate.InitialOutcome.Message, "multiple registry")
}

func TestPruneMergedInventoryUsesReadOnlyGitFactsAndUpstreamIdentity(t *testing.T) {
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/octocat/widget.git")
	runTUITestGit(t, repositoryRoot, "remote", "add", "fork", "git@github.com:octocat/widget.git")
	runTUITestGit(t, repositoryRoot, "remote", "add", "worktree-fork", "git@github.com:hubot/widget.git")
	branch := "feature/local-topic"
	worktreePath := filepath.Join(t.TempDir(), "ordinary-topic")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "config", "branch."+branch+".remote", "fork")
	runTUITestGit(t, repositoryRoot, "config", "branch."+branch+".merge", "refs/heads/topic")
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	runTUITestGit(t, repositoryRoot, "config", "extensions.worktreeConfig", "true")
	runTUITestGit(t, worktreePath, "config", "--worktree", "branch."+branch+".remote", "worktree-fork")
	runTUITestGit(t, worktreePath, "config", "--worktree", "branch."+branch+".merge", "refs/heads/worktree-topic")
	require.NoError(t, os.WriteFile(
		filepath.Join(repositoryRoot, ".git", "info", "exclude"),
		[]byte("valuable.local\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreePath, "valuable.local"),
		[]byte("keep me\n"),
		0o644,
	))
	generation, err := git.New(repositoryRoot).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	entry := &registry.WorktreeEntry{
		Path: worktreePath, Generation: generation,
		Repository: "https://github.com/octocat/widget.git",
	}
	cfg := &models.Config{Projects: []models.Project{{Path: repositoryRoot, Repository: commandBaseRepo}}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, []*registry.WorktreeEntry{entry},
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1, "main worktrees are outside merged-prune candidate inventory")
	var candidate *pruneMergedCandidate
	for index := range candidates {
		if utils.PathKey(candidates[index].Policy.Path) == utils.PathKey(worktreePath) {
			candidate = &candidates[index]
			break
		}
	}
	require.NotNil(t, candidate, "candidates=%+v", candidates)
	assert.Equal(t, commandBaseRepo, candidate.Policy.ProjectRepository)
	assert.Equal(t, commandSourceRepo, candidate.Policy.LiveRepository)
	assert.Equal(t, "github.com/hubot/widget", candidate.Policy.SourceRepository)
	assert.Equal(t, "worktree-topic", candidate.Policy.SourceBranch)
	assert.Equal(t, utils.CanonicalPath(repositoryRoot), candidate.Policy.MainRepositoryPath)
	assert.Equal(t, generation, candidate.Policy.Generation)
	assert.True(t, candidate.Policy.Dirty)
	assert.True(t, candidate.RegistryExpected)
	assert.Nil(t, candidate.InitialOutcome)
}

func TestPruneMergedInventoryRejectsMismatchedWorktreeBacklink(t *testing.T) {
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(
		t, repositoryRoot, "remote", "add", "origin",
		"https://github.com/octocat/widget.git",
	)
	firstPath := filepath.Join(t.TempDir(), "first")
	secondPath := filepath.Join(t.TempDir(), "second")
	runTUITestGit(t, repositoryRoot, "worktree", "add", "-b", "first", firstPath)
	runTUITestGit(t, repositoryRoot, "worktree", "add", "-b", "second", secondPath)
	_, err := git.New(repositoryRoot).ListWorktrees()
	require.NoError(t, err)
	secondGitDir := strings.TrimSpace(
		runTUITestGitOutput(t, secondPath, "rev-parse", "--absolute-git-dir"),
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(firstPath, ".git"),
		[]byte("gitdir: "+secondGitDir+"\n"),
		0o644,
	))
	cfg := &models.Config{Projects: []models.Project{{
		Path: repositoryRoot, Repository: commandBaseRepo,
	}}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	var candidate *pruneMergedCandidate
	for index := range candidates {
		if utils.PathKey(candidates[index].Policy.Path) == utils.PathKey(firstPath) {
			candidate = &candidates[index]
			break
		}
	}
	require.NotNil(t, candidate)
	require.NotNil(t, candidate.InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, candidate.InitialOutcome.Reason)
	assert.Contains(t, candidate.InitialOutcome.Message, "backlink")
	assert.Empty(t, candidate.Policy.LiveRepository)
	assert.Empty(t, candidate.Policy.SourceRepository)
	assert.Empty(t, candidate.Policy.SourceBranch)
}

func TestPruneMergedInventoryRejectsDistinctRootClaimingCandidateBacklink(
	t *testing.T,
) {
	for _, source := range []string{"global", "registry"} {
		t.Run(source, func(t *testing.T) {
			resetPruneMergedDeps(t)
			repositoryRoot := newTUITestRepo(t)
			runTUITestGit(
				t, repositoryRoot, "remote", "add", "origin",
				"https://github.com/acme/widget.git",
			)
			worktreePath := filepath.Join(t.TempDir(), "real-topic")
			runTUITestGit(
				t, repositoryRoot, "worktree", "add", "-b", "real-topic",
				worktreePath,
			)
			generation, err := git.New(repositoryRoot).WorktreeGeneration(worktreePath)
			require.NoError(t, err)
			gitDir := strings.TrimSpace(
				runTUITestGitOutput(t, worktreePath, "rev-parse", "--absolute-git-dir"),
			)
			copyPath := filepath.Join(t.TempDir(), "copied-topic")
			require.NoError(t, os.Mkdir(copyPath, 0o755))
			require.NoError(t, os.WriteFile(
				filepath.Join(copyPath, ".git"),
				[]byte("gitdir: "+gitDir+"\n"),
				0o644,
			))
			cfg := &models.Config{Projects: []models.Project{{
				Path: repositoryRoot, Repository: commandBaseRepo,
			}}}
			var entries []*registry.WorktreeEntry
			if source == "global" {
				cfg.Worktree.BaseDir = filepath.Dir(copyPath)
				findPruneMergedGlobalPaths = func(string) ([]string, error) {
					return []string{copyPath}, nil
				}
			} else {
				entries = []*registry.WorktreeEntry{{
					Path: copyPath, Repository: commandBaseRepo,
					Generation: generation,
				}}
			}

			candidates, err := defaultInspectPruneMergedCandidates(
				context.Background(), cfg, nil, entries,
			)

			require.NoError(t, err)
			var candidate *pruneMergedCandidate
			for index := range candidates {
				if utils.PathKey(candidates[index].Policy.Path) == utils.PathKey(worktreePath) {
					candidate = &candidates[index]
					break
				}
			}
			require.NotNil(t, candidate)
			require.NotNil(t, candidate.InitialOutcome)
			assert.Equal(t, prunepolicy.DoctorRequired, candidate.InitialOutcome.Reason)
			assert.Contains(t, candidate.InitialOutcome.Message, "claimed")
			assert.Equal(
				t, utils.CanonicalPath(copyPath),
				candidate.InitialOutcome.Evidence["claimants"],
			)
		})
	}
}

func TestPruneMergedInventoryReadsOriginFromLinkedWorktree(t *testing.T) {
	repositoryRoot := newTUITestRepo(t)
	branch := "feature/worktree-origin"
	worktreePath := filepath.Join(t.TempDir(), "worktree-origin")
	worktreeConfig := filepath.Join(t.TempDir(), "worktree-origin.config")
	runTUITestGit(t, repositoryRoot, "config", "--file", worktreeConfig, "remote.origin.url", "https://github.com/hubot/widget.git")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	worktreeGitDir := strings.TrimSpace(runTUITestGitOutput(t, worktreePath, "rev-parse", "--absolute-git-dir"))
	runTUITestGit(
		t, repositoryRoot, "config", "--add",
		"includeIf.gitdir:"+worktreeGitDir+".path", worktreeConfig,
	)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/octocat/widget.git")
	cfg := &models.Config{Projects: []models.Project{{
		Path: repositoryRoot, Repository: commandBaseRepo,
	}}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, commandBaseRepo, candidates[0].Policy.ProjectRepository)
	assert.Equal(t, "github.com/hubot/widget", candidates[0].Policy.LiveRepository)
}

func TestPruneMergedInventorySupportsSeparateGitDirectory(t *testing.T) {
	mainPath, linkedPath := newPruneMergedSeparateGitRepository(t)
	cfg := &models.Config{Projects: []models.Project{{
		Path: mainPath, Repository: commandBaseRepo,
	}}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, utils.PathKey(linkedPath), utils.PathKey(candidates[0].Policy.Path))
	assert.Equal(t, utils.PathKey(mainPath), utils.PathKey(candidates[0].RepositoryRoot))
	assert.Equal(t, utils.PathKey(mainPath), utils.PathKey(candidates[0].Policy.MainRepositoryPath))
}

func newPruneMergedSeparateGitRepository(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	mainPath := filepath.Join(base, "main-worktree")
	linkedPath := filepath.Join(base, "linked-worktree")
	separateGitDir := filepath.Join(base, "repository.git")
	runTUITestGit(
		t, "", "init", "-b", "main", "--separate-git-dir",
		separateGitDir, mainPath,
	)
	runTUITestGit(t, mainPath, "config", "user.name", "Test User")
	runTUITestGit(t, mainPath, "config", "user.email", "test@example.com")
	runTUITestGit(
		t, mainPath, "remote", "add", "origin",
		"https://github.com/acme/widget.git",
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(mainPath, "README.md"),
		[]byte("# Separate Git directory\n"),
		0o644,
	))
	runTUITestGit(t, mainPath, "add", "README.md")
	runTUITestGit(t, mainPath, "commit", "-m", "Initial commit")
	runTUITestGit(t, mainPath, "branch", "topic")
	runTUITestGit(t, mainPath, "worktree", "add", linkedPath, "topic")
	return mainPath, linkedPath
}

func TestPruneMergedSeparateGitDirectoryIncludesConfiguredLinkedIdentityClaim(
	t *testing.T,
) {
	mainPath, linkedPath := newPruneMergedSeparateGitRepository(t)
	cfg := &models.Config{Projects: []models.Project{
		{Path: mainPath, Repository: commandBaseRepo},
		{Path: linkedPath, Repository: "github.com/other/widget"},
	}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.NotNil(t, candidates[0].InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, candidates[0].InitialOutcome.Reason)
	assert.Contains(
		t,
		candidates[0].InitialOutcome.Message,
		"conflicting configured project repository identities",
	)
}

func TestPruneMergedConfiguredRootDoesNotFallBackToLiveOrigin(t *testing.T) {
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/acme/widget.git")
	branch := "feature/invalid-configured-identity"
	worktreePath := filepath.Join(t.TempDir(), "invalid-configured-identity")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	cfg := &models.Config{Projects: []models.Project{{
		Path: repositoryRoot, Repository: filepath.Join(t.TempDir(), "local-repository"),
	}}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Empty(t, candidates[0].Policy.ProjectRepository)
	assert.Equal(t, commandBaseRepo, candidates[0].Policy.LiveRepository)
	require.NotNil(t, candidates[0].InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, candidates[0].InitialOutcome.Reason)
	assert.Contains(t, candidates[0].InitialOutcome.Message, "configured project repository identity")
}

func TestPruneMergedConfiguredIdentityClaimsForCanonicalPath(t *testing.T) {
	tests := []struct {
		name             string
		secondRepository func(*testing.T) string
		wantDoctor       bool
	}{
		{
			name: "equivalent canonical identity",
			secondRepository: func(*testing.T) string {
				return "https://github.com/acme/widget.git"
			},
		},
		{
			name: "conflicting canonical identity",
			secondRepository: func(*testing.T) string {
				return "github.com/other/widget"
			},
			wantDoctor: true,
		},
		{
			name: "invalid competing identity",
			secondRepository: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "local-repository")
			},
			wantDoctor: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repositoryRoot := newTUITestRepo(t)
			runTUITestGit(
				t, repositoryRoot, "remote", "add", "origin",
				"https://github.com/acme/widget.git",
			)
			branch := "feature/duplicate-configured-identity"
			worktreePath := filepath.Join(t.TempDir(), "duplicate-configured-identity")
			runTUITestGit(t, repositoryRoot, "branch", branch)
			runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
			cfg := &models.Config{Projects: []models.Project{
				{Path: repositoryRoot, Repository: commandBaseRepo},
				{
					Path:       filepath.Join(repositoryRoot, "."),
					Repository: tt.secondRepository(t),
				},
			}}

			candidates, err := defaultInspectPruneMergedCandidates(
				context.Background(), cfg, nil, nil,
			)

			require.NoError(t, err)
			require.Len(t, candidates, 1)
			if tt.wantDoctor {
				require.NotNil(t, candidates[0].InitialOutcome)
				assert.Equal(
					t,
					prunepolicy.DoctorRequired,
					candidates[0].InitialOutcome.Reason,
				)
				assert.Contains(
					t,
					candidates[0].InitialOutcome.Message,
					"conflicting configured project repository identities",
				)
				return
			}
			assert.Nil(t, candidates[0].InitialOutcome)
			assert.Equal(t, commandBaseRepo, candidates[0].Policy.ProjectRepository)
		})
	}
}

func TestPruneMergedConflictingConfiguredIdentitiesForOneCommonRepository(
	t *testing.T,
) {
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(
		t, repositoryRoot, "remote", "add", "origin",
		"https://github.com/acme/widget.git",
	)
	branch := "feature/common-repository-conflict"
	worktreePath := filepath.Join(t.TempDir(), "common-repository-conflict")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	cfg := &models.Config{Projects: []models.Project{
		{Path: repositoryRoot, Repository: commandBaseRepo},
		{Path: worktreePath, Repository: "github.com/other/widget"},
	}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.NotNil(t, candidates[0].InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, candidates[0].InitialOutcome.Reason)
	assert.Contains(
		t,
		candidates[0].InitialOutcome.Message,
		"conflicting configured project repository identities",
	)
}

func TestPruneMergedUnconfiguredCandidateRequiresDoctorWhenConfiguredRootIsUnavailable(t *testing.T) {
	resetPruneMergedDeps(t)
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/octocat/widget.git")
	branch := "feature/rediscovered-fork"
	worktreePath := filepath.Join(t.TempDir(), "rediscovered-fork")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	missingConfiguredRoot := filepath.Join(t.TempDir(), "missing-upstream")
	findPruneMergedGlobalPaths = func(string) ([]string, error) {
		return []string{worktreePath}, nil
	}
	cfg := &models.Config{
		Projects: []models.Project{{
			Path: missingConfiguredRoot, Repository: commandBaseRepo,
		}},
		Worktree: models.WorktreeConfig{BaseDir: filepath.Dir(worktreePath)},
	}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 2)
	byPath := make(map[string]pruneMergedCandidate, len(candidates))
	for _, candidate := range candidates {
		byPath[utils.PathKey(candidate.Policy.Path)] = candidate
	}
	unavailable := byPath[utils.PathKey(missingConfiguredRoot)]
	require.NotNil(t, unavailable.InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, unavailable.InitialOutcome.Reason)
	rediscovered := byPath[utils.PathKey(worktreePath)]
	assert.Equal(t, "github.com/octocat/widget", rediscovered.Policy.ProjectRepository)
	require.NotNil(t, rediscovered.InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, rediscovered.InitialOutcome.Reason)
	assert.Contains(t, rediscovered.InitialOutcome.Message, "configured project")
}

func TestPruneMergedUnconfiguredCandidateRequiresDoctorWhenConfiguredPathIsEmpty(
	t *testing.T,
) {
	resetPruneMergedDeps(t)
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(
		t, repositoryRoot, "remote", "add", "origin",
		"https://github.com/octocat/widget.git",
	)
	branch := "feature/empty-configured-path"
	worktreePath := filepath.Join(t.TempDir(), "empty-configured-path")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	findPruneMergedGlobalPaths = func(string) ([]string, error) {
		return []string{worktreePath}, nil
	}
	cfg := &models.Config{
		Projects: []models.Project{{
			Path: " \t", Repository: commandBaseRepo,
		}},
		Worktree: models.WorktreeConfig{BaseDir: filepath.Dir(worktreePath)},
	}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, "github.com/octocat/widget", candidates[0].Policy.ProjectRepository)
	require.NotNil(t, candidates[0].InitialOutcome)
	assert.Equal(t, prunepolicy.DoctorRequired, candidates[0].InitialOutcome.Reason)
	assert.Contains(t, candidates[0].InitialOutcome.Message, "configured project")
}

func TestPruneMergedConfiguredCandidateRemainsEligibleWhenAnotherConfiguredRootIsUnavailable(t *testing.T) {
	repositoryRoot := newTUITestRepo(t)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/octocat/widget.git")
	branch := "feature/healthy-configured"
	worktreePath := filepath.Join(t.TempDir(), "healthy-configured")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	missingConfiguredRoot := filepath.Join(t.TempDir(), "missing-project")
	cfg := &models.Config{Projects: []models.Project{
		{Path: missingConfiguredRoot, Repository: "github.com/acme/missing"},
		{Path: repositoryRoot, Repository: commandBaseRepo},
	}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 2)
	byPath := make(map[string]pruneMergedCandidate, len(candidates))
	for _, candidate := range candidates {
		byPath[utils.PathKey(candidate.Policy.Path)] = candidate
	}
	healthy := byPath[utils.PathKey(worktreePath)]
	assert.Equal(t, commandBaseRepo, healthy.Policy.ProjectRepository)
	assert.Equal(t, "github.com/octocat/widget", healthy.Policy.LiveRepository)
	assert.Nil(t, healthy.InitialOutcome)
}

func TestPruneMergedInventoryStripsCredentialsFromWorktreeGitProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test probe uses a POSIX executable script")
	}
	repositoryRoot := newTUITestRepo(t)
	branch := "feature/credential-probe"
	worktreePath := filepath.Join(t.TempDir(), "credential-probe")
	runTUITestGit(t, repositoryRoot, "branch", branch)
	runTUITestGit(t, repositoryRoot, "worktree", "add", worktreePath, branch)
	probeOutput := filepath.Join(t.TempDir(), "probe-output")
	probeScript := filepath.Join(t.TempDir(), "fsmonitor-probe")
	require.NoError(t, os.WriteFile(probeScript, []byte(
		"#!/bin/sh\n"+
			"printf '%s|%s' \"${KWT_GITHUB_TOKEN-unset}\" \"${CUSTOM_FLEET_TOKEN-unset}\" > \"$KWT_PROBE_OUTPUT\"\n",
	), 0o755))
	runTUITestGit(t, repositoryRoot, "config", "core.fsmonitor", probeScript)
	_, err := git.New(repositoryRoot).EnsureWorktreeGeneration(worktreePath)
	require.NoError(t, err)
	t.Setenv("KWT_PROBE_OUTPUT", probeOutput)
	t.Setenv("KWT_GITHUB_TOKEN", "github-secret")
	t.Setenv("CUSTOM_FLEET_TOKEN", "fleet-secret")
	cfg := &models.Config{
		Projects: []models.Project{{Path: repositoryRoot, Repository: commandBaseRepo}},
		Fleet:    models.FleetConfig{TokenEnv: "CUSTOM_FLEET_TOKEN"},
	}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	probeContents, err := os.ReadFile(probeOutput)
	require.NoError(t, err, "configured fsmonitor probe was not invoked")
	assert.Equal(t, "unset|unset", string(probeContents))
	require.NoError(t, os.WriteFile(probeOutput, []byte("not-invoked"), 0o600))

	require.NoError(t, defaultValidatePruneMergedWorktree(candidates[0]))
	probeContents, err = os.ReadFile(probeOutput)
	require.NoError(t, err)
	assert.Equal(t, "unset|unset", string(probeContents))
	require.NoError(t, os.WriteFile(probeOutput, []byte("not-invoked"), 0o600))

	removed, err := defaultRemovePruneMergedWorktree(
		candidates[0],
		func(remove func() error) (bool, error) {
			err := remove()
			return err == nil || git.WorktreeWasRemoved(err), err
		},
	)
	require.NoError(t, err)
	assert.True(t, removed)
	probeContents, err = os.ReadFile(probeOutput)
	require.NoError(t, err)
	assert.Equal(t, "unset|unset", string(probeContents))
	assert.NoDirExists(t, worktreePath)
}

func TestPruneMergedGlobalInventoryDerivesBaseFromEachWorktreeOrigin(t *testing.T) {
	resetPruneMergedDeps(t)
	repositoryRoot := newTUITestRepo(t)
	globalBase := t.TempDir()
	firstPath := filepath.Join(globalBase, "first-origin")
	secondPath := filepath.Join(globalBase, "second-origin")
	firstConfig := filepath.Join(t.TempDir(), "first-origin.config")
	secondConfig := filepath.Join(t.TempDir(), "second-origin.config")
	runTUITestGit(t, repositoryRoot, "config", "--file", firstConfig, "remote.origin.url", "https://github.com/octocat/widget.git")
	runTUITestGit(t, repositoryRoot, "config", "--file", secondConfig, "remote.origin.url", "https://github.com/hubot/widget.git")
	runTUITestGit(t, repositoryRoot, "branch", "feature/first-origin")
	runTUITestGit(t, repositoryRoot, "branch", "feature/second-origin")
	runTUITestGit(t, repositoryRoot, "worktree", "add", firstPath, "feature/first-origin")
	runTUITestGit(t, repositoryRoot, "worktree", "add", secondPath, "feature/second-origin")
	firstGitDir := strings.TrimSpace(runTUITestGitOutput(t, firstPath, "rev-parse", "--absolute-git-dir"))
	secondGitDir := strings.TrimSpace(runTUITestGitOutput(t, secondPath, "rev-parse", "--absolute-git-dir"))
	runTUITestGit(
		t, repositoryRoot, "config", "--add",
		"includeIf.gitdir:"+firstGitDir+".path", firstConfig,
	)
	runTUITestGit(
		t, repositoryRoot, "config", "--add",
		"includeIf.gitdir:"+secondGitDir+".path", secondConfig,
	)
	runTUITestGit(t, repositoryRoot, "remote", "add", "origin", "https://github.com/acme/widget.git")
	findPruneMergedGlobalPaths = func(string) ([]string, error) {
		return []string{firstPath, secondPath}, nil
	}
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: globalBase}}

	candidates, err := defaultInspectPruneMergedCandidates(
		context.Background(), cfg, nil, nil,
	)

	require.NoError(t, err)
	require.Len(t, candidates, 2)
	byPath := make(map[string]pruneMergedCandidate, len(candidates))
	for _, candidate := range candidates {
		byPath[utils.PathKey(candidate.Policy.Path)] = candidate
	}
	assert.Equal(t, "github.com/octocat/widget", byPath[utils.PathKey(firstPath)].Policy.LiveRepository)
	assert.Equal(t, "github.com/octocat/widget", byPath[utils.PathKey(firstPath)].Policy.ProjectRepository)
	assert.Equal(t, "github.com/hubot/widget", byPath[utils.PathKey(secondPath)].Policy.LiveRepository)
	assert.Equal(t, "github.com/hubot/widget", byPath[utils.PathKey(secondPath)].Policy.ProjectRepository)
}

func resetPruneMergedCommand(t *testing.T) {
	t.Helper()
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	resetPruneMergedDeps(t)
	pruneMerged = true
	loadPruneMergedConfig = func() (*models.Config, error) { return &models.Config{}, nil }
	openPruneMergedRegistry = func() (pruneMergedRegistry, error) {
		return &fakePruneMergedRegistry{removed: true}, nil
	}
	openPruneMergedProvenanceStore = func() pruneMergedProvenanceStore {
		return &fakePruneMergedProvenance{removeResult: true}
	}
	removePruneMergedWorktree = fakePruneMergedRemoval(
		func(pruneMergedCandidate) error { return nil },
	)
}

func resetPruneMergedDeps(t *testing.T) {
	t.Helper()
	oldLoad := loadPruneMergedConfig
	oldRegistry := openPruneMergedRegistry
	oldStore := openPruneMergedProvenanceStore
	oldInspect := inspectPruneMergedCandidates
	oldProvider := newPruneMergedProvider
	oldValidate := validatePruneMergedWorktree
	oldRemove := removePruneMergedWorktree
	oldFindGlobalPaths := findPruneMergedGlobalPaths
	t.Cleanup(func() {
		loadPruneMergedConfig = oldLoad
		openPruneMergedRegistry = oldRegistry
		openPruneMergedProvenanceStore = oldStore
		inspectPruneMergedCandidates = oldInspect
		newPruneMergedProvider = oldProvider
		validatePruneMergedWorktree = oldValidate
		removePruneMergedWorktree = oldRemove
		findPruneMergedGlobalPaths = oldFindGlobalPaths
	})
}

func setPruneMergedInventory(candidates ...pruneMergedCandidate) {
	inspectPruneMergedCandidates = func(context.Context, *models.Config, map[string]pullrequest.Provenance, []*registry.WorktreeEntry) ([]pruneMergedCandidate, error) {
		return candidates, nil
	}
}

func setPruneMergedProvider(provider prunepolicy.MergedProvider) {
	newPruneMergedProvider = func(context.Context) (prunepolicy.MergedProvider, error) { return provider, nil }
}

func commandMergedCandidate(path, head string) pruneMergedCandidate {
	return pruneMergedCandidate{
		RepositoryRoot: "/projects/widget",
		Policy: prunepolicy.MergedCandidate{
			Path: path, Branch: "topic-local", Head: head, Generation: "0123456789abcdef0123456789abcdef",
			MainRepositoryPath: "/projects/widget",
			ProjectRepository:  commandBaseRepo, LiveRepository: commandBaseRepo,
			SourceRepository: commandSourceRepo, SourceBranch: "topic",
		},
	}
}

func commandMergedProvenance(candidate pruneMergedCandidate) pullrequest.Provenance {
	return pullrequest.Provenance{
		PullRequestID: pullrequest.OpaqueID(commandBaseRepo, 17), Provider: "github",
		Repository: commandBaseRepo, Number: 17,
		URL: "https://github.com/acme/widget/pull/17", HeadSHA: candidate.Policy.Head,
		SourceRepo: commandSourceRepo, SourceBranch: candidate.Policy.SourceBranch,
		Project: pullrequest.Project{Identity: commandBaseRepo, Path: candidate.RepositoryRoot},
		Workspace: pullrequest.Workspace{
			ID: "workspace-17", Repository: commandBaseRepo,
			Branch: candidate.Policy.Branch, Path: candidate.Policy.Path, State: "ready",
		},
	}
}

func providerForCommandCandidates(candidates ...pruneMergedCandidate) *fakePruneMergedProvider {
	byHead := make(map[string]pruneMergedCandidate, len(candidates))
	byNumber := make(map[int]pruneMergedCandidate)
	for _, candidate := range candidates {
		byHead[candidate.Policy.Head] = candidate
		if candidate.Policy.Provenance != nil {
			byNumber[candidate.Policy.Provenance.Number] = candidate
		}
	}
	return &fakePruneMergedProvider{
		forCommit: func(sha string) ([]pullrequest.PullRequest, error) {
			candidate, ok := byHead[sha]
			if !ok {
				return nil, nil
			}
			return []pullrequest.PullRequest{commandMergedPR(candidate)}, nil
		},
		get: func(number int) (pullrequest.PullRequest, error) {
			candidate, ok := byNumber[number]
			if !ok {
				return pullrequest.PullRequest{}, fmt.Errorf("unexpected PR #%d", number)
			}
			return commandMergedPR(candidate), nil
		},
	}
}

func commandMergedPR(candidate pruneMergedCandidate) pullrequest.PullRequest {
	mergedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return pullrequest.PullRequest{
		ID: pullrequest.OpaqueID(commandBaseRepo, 17), Provider: "github",
		Repository: commandRepository(commandBaseRepo), Number: 17,
		URL: "https://github.com/acme/widget/pull/17", Title: "Topic", Author: "octocat",
		Source: pullrequest.Branch{Name: candidate.Policy.SourceBranch, Repository: commandRepository(candidate.Policy.SourceRepository)},
		Target: pullrequest.Branch{Name: "main", Repository: commandRepository(commandBaseRepo)},
		State:  "closed", HeadSHA: candidate.Policy.Head, MergedAt: &mergedAt,
	}
}

func commandRepository(identity string) pullrequest.Repository {
	if identity == commandSourceRepo {
		return pullrequest.Repository{Provider: "github", Identity: identity, Host: "github.com", Owner: "octocat", Name: "widget"}
	}
	return pullrequest.Repository{Provider: "github", Identity: identity, Host: "github.com", Owner: "acme", Name: "widget"}
}
