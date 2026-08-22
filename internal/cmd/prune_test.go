package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/prunepolicy"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

func TestPruneRequiresPolicy(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	cmd, _, stderr := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 2)
	assert.Contains(t, stderr.String(), "kwt doctor --fix")
}

// TestPruneCmdIsolatesFromCwdConfig guards the global-scope invariant:
// pruneCmd must not inherit root's PersistentPreRunE, which merges the caller's
// repository-local .kwt.toml and can redirect the fleet worktree base directory.
func TestPruneCmdIsolatesFromCwdConfig(t *testing.T) {
	oldMerge := mergeCwdLocal
	t.Cleanup(func() { mergeCwdLocal = oldMerge })
	mergeCwdLocal = func() error {
		return errors.New("repository-local configuration must not be merged")
	}
	require.NotNil(t, pruneCmd.PersistentPreRunE,
		"prune must define its own PersistentPreRunE to bypass root's cwd merge")
	require.NoError(t, pruneCmd.PersistentPreRunE(pruneCmd, nil))
}

func TestPruneInitializationFailureUsesMaintenanceErrorContract(t *testing.T) {
	resetPruneCommandFlags(t)
	pruneJSON = true
	oldInitErr := configInitErr
	configInitErr = errors.New("config unavailable")
	t.Cleanup(func() { configInitErr = oldInitErr })
	cmd, stdout, stderr := fleetTestCommand()

	err := pruneCmd.PersistentPreRunE(cmd, nil)

	assertExitCode(t, err, 2)
	var envelope maintenanceErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, "prune", envelope.Command)
	assert.Equal(t, "initialization_failed", envelope.Error.Code)
	assert.Contains(t, stderr.String(), "config unavailable")
}

func TestPruneHelpDescribesExplicitPoliciesAndMigration(t *testing.T) {
	var output bytes.Buffer
	pruneCmd.SetOut(&output)
	t.Cleanup(func() { pruneCmd.SetOut(nil) })

	require.NoError(t, pruneCmd.Help())

	help := output.String()
	assert.Contains(t, help, "--expired")
	assert.Contains(t, help, "--merged")
	assert.Contains(t, help, "--dry-run")
	assert.Contains(t, help, "--json")
	assert.Contains(t, help, "clean worktrees")
	assert.Contains(t, help, "bare kwt prune")
	assert.Contains(t, help, "kwt doctor --fix")
	assert.Contains(t, help, "Git 2.31")
}

func TestPruneRejectsMultiplePolicies(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	pruneExpired = true
	pruneMerged = true
	cmd, _, stderr := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 2)
	assert.Contains(t, stderr.String(), "mutually exclusive")
}

func TestPruneMergedRejectsForce(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	pruneMerged = true
	pruneForce = true
	cmd, _, stderr := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 2)
	assert.Contains(t, stderr.String(), "--force")
}

func TestPrunePoliciesRejectUnsupportedGitVersion(t *testing.T) {
	tests := []struct {
		name    string
		policy  func()
		jsonOut bool
	}{
		{name: "expired human", policy: func() { pruneExpired = true }},
		{name: "merged json", policy: func() { pruneMerged = true }, jsonOut: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetPruneCommandFlags(t)
			tt.policy()
			pruneJSON = tt.jsonOut
			requireMaintenanceGitVersion = func() error {
				return errors.New("Git 2.31 or newer is required; found Git 2.30.2")
			}
			cmd, stdout, stderr := fleetTestCommand()

			err := runPrune(cmd, nil)

			assertExitCode(t, err, 2)
			assert.Contains(t, stderr.String(), "unsupported_git_version")
			if tt.jsonOut {
				var envelope maintenanceErrorEnvelope
				require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
				assert.Equal(t, "prune", envelope.Command)
				assert.Equal(t, "unsupported_git_version", envelope.Error.Code)
			} else {
				assert.Empty(t, stdout.String())
			}
		})
	}
}

func TestPruneInvalidUsageWinsOverGitVersionCheck(t *testing.T) {
	tests := []struct {
		name   string
		setup  func()
		needle string
	}{
		{name: "policy required", setup: func() {}, needle: "policy_required"},
		{
			name:   "multiple policies",
			setup:  func() { pruneExpired, pruneMerged = true, true },
			needle: "incompatible_policies",
		},
		{
			name:   "merged force",
			setup:  func() { pruneMerged, pruneForce = true, true },
			needle: "incompatible_flags",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetPruneCommandFlags(t)
			tt.setup()
			var versionChecks int
			requireMaintenanceGitVersion = func() error {
				versionChecks++
				return errors.New("unsupported Git")
			}
			cmd, _, stderr := fleetTestCommand()

			err := runPrune(cmd, nil)

			assertExitCode(t, err, 2)
			assert.Zero(t, versionChecks)
			assert.Contains(t, stderr.String(), tt.needle)
		})
	}
}

func TestPruneExpiredMissingPathReportsDoctorRequired(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	initCommandTestConfig(t, t.TempDir())
	pruneExpired = true
	expiredPath := filepath.Join(t.TempDir(), "missing-expired")
	registerExpiredWorktree(t, expiredPath, "0123456789abcdef0123456789abcdef")
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.DoctorRequired))
	assert.Contains(t, stdout.String(), "kwt doctor --fix")
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, exists := reg.Get(expiredPath)
	assert.True(t, exists)
}

func TestPruneExpiredMissingGenerationReportsReason(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "legacy-expired")
	runTUITestGit(t, repoPath, "branch", "feature/legacy-expired")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/legacy-expired")
	registerExpiredWorktree(t, worktreePath, "")
	pruneExpired = true
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.MissingGeneration))
	assert.Contains(t, stdout.String(), "kwt doctor --fix")
	assert.DirExists(t, worktreePath)
}

func TestPruneExpiredJSONContainsStableReasons(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	initCommandTestConfig(t, t.TempDir())
	pruneExpired = true
	pruneJSON = true
	expiredPath := filepath.Join(t.TempDir(), "missing-expired")
	registerExpiredWorktree(t, expiredPath, "0123456789abcdef0123456789abcdef")
	cmd, stdout, stderr := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, prunepolicy.SchemaVersion, report.SchemaVersion)
	assert.Equal(t, "expired", report.Policy)
	require.Len(t, report.Outcomes, 1)
	assert.Equal(t, prunepolicy.DoctorRequired, report.Outcomes[0].Reason)
	assert.NotContains(t, stdout.String(), "Expired worktree:")
	assert.Contains(t, stderr.String(), "kwt: validate candidates")
}

func TestPruneExpiredForceStillRequiresGeneration(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "legacy-force")
	runTUITestGit(t, repoPath, "branch", "feature/legacy-force")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/legacy-force")
	registerExpiredWorktree(t, worktreePath, "")
	pruneExpired = true
	pruneForce = true
	cmd, stdout, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.MissingGeneration))
	assert.DirExists(t, worktreePath)
}

func TestPruneExpiredDryRunReportsDirtyWorktree(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "dirty-expired")
	runTUITestGit(t, repoPath, "branch", "feature/dirty-expired")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/dirty-expired")
	generation, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty\n"), 0o644))
	registerExpiredWorktree(t, worktreePath, generation)
	pruneExpired = true
	pruneDryRun = true
	cmd, stdout, _ := fleetTestCommand()

	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.DirtyWorktree))
	assert.DirExists(t, worktreePath)
}

func TestPruneExpiredRejectsMismatchedWorktreeBacklink(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repositoryRoot := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	firstPath := filepath.Join(t.TempDir(), "expired-first")
	secondPath := filepath.Join(t.TempDir(), "expired-second")
	runTUITestGit(
		t, repositoryRoot, "worktree", "add", "-b", "expired-first", firstPath,
	)
	runTUITestGit(
		t, repositoryRoot, "worktree", "add", "-b", "expired-second", secondPath,
	)
	generation, err := git.New(repositoryRoot).WorktreeGeneration(firstPath)
	require.NoError(t, err)
	registerExpiredWorktree(t, firstPath, generation)
	secondGitDir := strings.TrimSpace(
		runTUITestGitOutput(t, secondPath, "rev-parse", "--absolute-git-dir"),
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(firstPath, ".git"),
		[]byte("gitdir: "+secondGitDir+"\n"),
		0o644,
	))
	pruneExpired = true
	pruneDryRun = true
	cmd, stdout, _ := fleetTestCommand()

	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.DoctorRequired))
	assert.Contains(t, stdout.String(), "backlink")
	assert.DirExists(t, firstPath)
}

func TestPruneExpiredCarriesVerifiedGitDirIntoValidation(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repositoryRoot := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "expired-verified-backlink")
	runTUITestGit(
		t, repositoryRoot, "worktree", "add", "-b", "expired-verified-backlink",
		worktreePath,
	)
	generation, err := git.New(repositoryRoot).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	registerExpiredWorktree(t, worktreePath, generation)
	expectedGitDir := strings.TrimSpace(
		runTUITestGitOutput(t, worktreePath, "rev-parse", "--absolute-git-dir"),
	)
	var captured git.WorktreeRemovalConditions
	validatePruneExpiredWorktree = func(
		_ *git.Git, _ string, conditions git.WorktreeRemovalConditions,
	) error {
		captured = conditions
		return &git.ConditionError{Reason: git.ReasonLocked, Path: worktreePath}
	}
	pruneExpired = true
	pruneDryRun = true
	cmd, _, _ := fleetTestCommand()

	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	require.NotEmpty(t, captured.ExpectedGitDir)
	assert.Equal(t, utils.PathKey(expectedGitDir), utils.PathKey(captured.ExpectedGitDir))
}

func TestPruneExpiredDryRunReportsLockedWorktree(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "locked-expired")
	runTUITestGit(t, repoPath, "branch", "feature/locked-expired")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/locked-expired")
	generation, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	registerExpiredWorktree(t, worktreePath, generation)
	runTUITestGit(t, repoPath, "worktree", "lock", "--reason", "maintenance", worktreePath)
	pruneExpired = true
	pruneDryRun = true
	pruneJSON = true
	cmd, stdout, stderr := fleetTestCommand()

	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Len(t, report.Outcomes, 1)
	assert.Equal(t, prunepolicy.LockedWorktree, report.Outcomes[0].Reason)
	assert.Contains(t, stderr.String(), "kwt: validate candidates")
	assert.DirExists(t, worktreePath)
}

func TestPruneExpiredDryRunRevalidatesMainWorktree(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	generation, err := git.New(repoPath).WorktreeGeneration(repoPath)
	require.NoError(t, err)
	registerExpiredWorktree(t, repoPath, generation)
	pruneExpired = true
	pruneDryRun = true
	pruneJSON = true
	cmd, stdout, stderr := fleetTestCommand()

	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	var report prunepolicy.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Len(t, report.Outcomes, 1)
	assert.Equal(t, prunepolicy.MainWorktree, report.Outcomes[0].Reason)
	assert.Contains(t, stderr.String(), "kwt: validate candidates")
	assert.DirExists(t, repoPath)
}

func TestPruneExpiredRemovesWorktreeWithIgnoredArtifact(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "ignored-expired")
	runTUITestGit(t, repoPath, "branch", "feature/ignored-expired")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/ignored-expired")
	generation, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(repoPath, ".git", "info", "exclude"),
		[]byte("build-output.local\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreePath, "build-output.local"),
		[]byte("generated\n"),
		0o644,
	))
	registerExpiredWorktree(t, worktreePath, generation)
	pruneExpired = true
	cmd, stdout, stderr := fleetTestCommand()

	err = runPrune(cmd, nil)

	require.NoError(t, err, "stdout=%s stderr=%s", stdout.String(), stderr.String())
	assert.NoDirExists(t, worktreePath)
}

func TestPruneExpiredPreservesReplacementGeneration(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "expired-replacement")
	runTUITestGit(t, repoPath, "branch", "feature/expired-original")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/expired-original")
	originalGeneration, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	registerExpiredWorktree(t, worktreePath, originalGeneration)
	runTUITestGit(t, repoPath, "worktree", "remove", "--force", worktreePath)
	runTUITestGit(t, repoPath, "branch", "feature/expired-replacement")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/expired-replacement")
	_, err = git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	pruneExpired = true
	pruneForce = true
	cmd, stdout, _ := fleetTestCommand()

	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.GenerationChanged))
	assert.DirExists(t, worktreePath)
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, exists := reg.Get(worktreePath)
	assert.True(t, exists)
}

func TestPruneExpiredPreservesWorktreeWhenExpirationChangesAfterInspection(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "expiration-extended")
	runTUITestGit(t, repoPath, "branch", "feature/expiration-extended")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/expiration-extended")
	generation, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	registerExpiredWorktree(t, worktreePath, generation)
	pruneExpired = true
	pruneForce = true
	validatePruneExpiredWorktree = func(
		g *git.Git, path string, conditions git.WorktreeRemovalConditions,
	) error {
		reg, registryErr := registry.New()
		require.NoError(t, registryErr)
		entry, ok := reg.Get(path)
		require.True(t, ok)
		future := time.Now().Add(time.Hour)
		entry.ExpiresAt = &future
		require.NoError(t, reg.Register(entry))
		return g.ValidateWorktreeRemoval(path, conditions)
	}
	cmd, stdout, _ := fleetTestCommand()

	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.ExpirationPolicyChanged))
	assert.DirExists(t, worktreePath)
}

func TestPruneExpiredRemovesMatchingGenerationAndPublishesOnce(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "expired-matching")
	runTUITestGit(t, repoPath, "branch", "feature/expired-matching")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/expired-matching")
	generation, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	registerExpiredWorktree(t, worktreePath, generation)
	pruneExpired = true
	pruneForce = true
	var publications int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, stdout, _ := fleetTestCommand()

	err = runPrune(cmd, nil)

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), string(prunepolicy.Removed))
	assert.NoDirExists(t, worktreePath)
	assert.Equal(t, 1, publications)
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, exists := reg.Get(worktreePath)
	assert.False(t, exists)
}

func TestPruneExpiredNoopDoesNotPublish(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	initCommandTestConfig(t, t.TempDir())
	pruneExpired = true
	var publications int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) { publications++ }
	cmd, _, _ := fleetTestCommand()

	err := runPrune(cmd, nil)

	require.NoError(t, err)
	assert.Zero(t, publications)
}

func TestPruneExpiredReportsBoundedPhases(t *testing.T) {
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)
	initCommandTestConfig(t, t.TempDir())
	oldTerminal := maintenanceProgressIsTerminal
	maintenanceProgressIsTerminal = func(io.Writer) bool { return false }
	t.Cleanup(func() { maintenanceProgressIsTerminal = oldTerminal })
	reg, err := registry.New()
	require.NoError(t, err)
	expiredAt := time.Now().Add(-time.Hour)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:      "/work/main",
		IsMain:    true,
		ExpiresAt: &expiredAt,
	}))
	pruneExpired = true
	cmd, _, stderr := fleetTestCommand()

	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	text := stderr.String()
	assert.Contains(t, text, "kwt: load candidates")
	assert.Contains(t, text, "kwt: validate candidates 1/1")
	assert.Contains(t, text, "kwt: remove candidates 1/1")
}

func TestPruneExpiredCompletesBookkeepingAfterGitDeregistersWithResidualFiles(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell wrapper")
	}
	resetFleetCommandDeps(t)
	resetPruneCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	worktreePath := filepath.Join(t.TempDir(), "expired-residual")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"task7/expired-residual",
		worktreePath,
	)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	expiredAt := time.Now().Add(-time.Hour)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "repo",
		Branch:     "task7/expired-residual",
		Path:       worktreePath,
		ExpiresAt:  &expiredAt,
		Generation: generation,
	}))
	pruneExpired = true

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	wrapper := `#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "remove" ]; then
	"$REAL_GIT" "$@" || exit $?
	mkdir -p "$3"
	printf 'created during removal\n' > "$3/residual"
	printf "error: failed to delete '%s': Directory not empty\n" "$3" >&2
	exit 1
fi
exec "$REAL_GIT" "$@"
`
	require.NoError(t, os.WriteFile(wrapperPath, []byte(wrapper), 0755))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	publishCalls := 0
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		publishCalls++
	}
	cmd, stdout, _ := fleetTestCommand()
	err = runPrune(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Contains(t, stdout.String(), string(prunepolicy.CleanupIncomplete))
	assert.Contains(t, stdout.String(), "worktree removed, but files remain at ")
	assert.Equal(t, 1, publishCalls)
	assert.FileExists(t, filepath.Join(worktreePath, "residual"))
	reloaded, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, exists := reloaded.Get(worktreePath)
	assert.False(t, exists)
}

func resetPruneCommandFlags(t *testing.T) {
	t.Helper()
	oldPruneExpired := pruneExpired
	oldPruneMerged := pruneMerged
	oldPruneDryRun := pruneDryRun
	oldPruneForce := pruneForce
	oldPruneJSON := pruneJSON
	oldSilenceUsage := rootCmd.SilenceUsage
	oldSilenceErrors := rootCmd.SilenceErrors
	oldOpenExpiredRegistry := openPruneExpiredRegistry
	oldValidateExpired := validatePruneExpiredWorktree
	oldRemoveExpired := removePruneExpiredWorktree
	oldRequireMaintenanceGitVersion := requireMaintenanceGitVersion
	t.Cleanup(func() {
		pruneExpired = oldPruneExpired
		pruneMerged = oldPruneMerged
		pruneDryRun = oldPruneDryRun
		pruneForce = oldPruneForce
		pruneJSON = oldPruneJSON
		rootCmd.SilenceUsage = oldSilenceUsage
		rootCmd.SilenceErrors = oldSilenceErrors
		openPruneExpiredRegistry = oldOpenExpiredRegistry
		validatePruneExpiredWorktree = oldValidateExpired
		removePruneExpiredWorktree = oldRemoveExpired
		requireMaintenanceGitVersion = oldRequireMaintenanceGitVersion
	})
	pruneExpired = false
	pruneMerged = false
	pruneDryRun = false
	pruneForce = false
	pruneJSON = false
	requireMaintenanceGitVersion = func() error { return nil }
}

func registerExpiredWorktree(t *testing.T, path string, generation string) {
	t.Helper()
	reg, err := registry.New()
	require.NoError(t, err)
	expiredAt := time.Now().Add(-time.Hour)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Repository: "repo", Branch: "expired", Path: path,
		ExpiresAt: &expiredAt, Generation: generation,
	}))
}
