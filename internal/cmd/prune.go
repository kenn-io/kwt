package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/prunepolicy"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/utils"
)

type pruneExpiredRegistry interface {
	ListExpired() []*registry.WorktreeEntry
	EntryMatches(string, *registry.WorktreeEntry) (bool, error)
	RemoveIfMatchAfter(string, *registry.WorktreeEntry, func() error) (bool, error)
}

var (
	openPruneExpiredRegistry = func() (pruneExpiredRegistry, error) {
		return registry.New()
	}
	validatePruneExpiredWorktree = func(g *git.Git, path string, conditions git.WorktreeRemovalConditions) error {
		return g.ValidateWorktreeRemoval(path, conditions)
	}
	removePruneExpiredWorktree = func(
		g *git.Git, path string, force bool, conditions git.WorktreeRemovalConditions,
		claim func(func() error) (bool, error),
	) (bool, error) {
		return g.RemoveWorktreeCheckedAfterClaim(path, force, conditions, claim)
	}
)

var (
	pruneExpired bool
	pruneMerged  bool
	pruneDryRun  bool
	pruneForce   bool
	pruneJSON    bool
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove live worktrees by an explicit policy",
	Long: `Remove live worktrees only when an explicit policy confirms them.

Use --expired for expiration-based removal or --merged for pull-request-based
removal. Structural Git metadata and registry cleanup belong to
kwt doctor --fix; bare kwt prune is no longer a mutation command. Prune
policies require Git 2.31 or newer.`,
	Example: `  # Preview expired worktrees
  kwt prune --expired --dry-run

  # Remove expired worktrees
  kwt prune --expired

  # Force removal of dirty expired worktrees
  kwt prune --expired --force

  # Preview and remove clean worktrees for merged pull requests
  kwt prune --merged --dry-run
  kwt prune --merged`,
	Args: cobra.NoArgs,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		// Prune operates across the global worktree fleet. Repository-local
		// configuration from the caller's cwd must not redirect that scope.
		if err := requireConfigInitialization(); err != nil {
			return writeMaintenanceError(
				cmd, "prune", "initialization_failed", err.Error(), 2, pruneJSON,
			)
		}
		return nil
	},
	RunE: runPrune,
}

func init() {
	rootCmd.AddCommand(pruneCmd)
	pruneCmd.Flags().BoolVar(&pruneExpired, "expired", false, "Remove expired live worktrees")
	pruneCmd.Flags().BoolVar(&pruneMerged, "merged", false, "Remove clean worktrees for explicitly merged pull requests")
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "Preview policy outcomes without removing")
	pruneCmd.Flags().BoolVar(&pruneForce, "force", false, "Allow dirty expiration-policy removal")
	pruneCmd.Flags().BoolVar(&pruneJSON, "json", false, "Output a machine-readable report")
}

func runPrune(cmd *cobra.Command, args []string) error {
	if pruneExpired && pruneMerged {
		return writeMaintenanceError(
			cmd, "prune", "incompatible_policies",
			"--expired and --merged are mutually exclusive", 2, pruneJSON,
		)
	}
	if pruneMerged && pruneForce {
		return writeMaintenanceError(
			cmd, "prune", "incompatible_flags",
			"--force is not available with --merged", 2, pruneJSON,
		)
	}
	if !pruneExpired && !pruneMerged {
		return writeMaintenanceError(
			cmd,
			"prune",
			"policy_required",
			"choose --expired or --merged for live removal; run kwt doctor --fix for structural metadata cleanup",
			2,
			pruneJSON,
		)
	}
	if err := checkMaintenanceGitVersion(cmd, "prune", pruneJSON); err != nil {
		return err
	}
	if pruneExpired {
		return runPruneExpired(cmd, args)
	}
	return runPruneMerged(cmd, args)
}

func runPruneExpired(cmd *cobra.Command, _ []string) error {
	progress := newMaintenanceProgress(cmd, true)
	defer progress.Close()
	progress.Phase("load candidates", 0)
	reg, err := openPruneExpiredRegistry()
	if err != nil {
		progress.Pause()
		return writeMaintenanceError(
			cmd, "prune", "inspection_failed",
			fmt.Sprintf("open worktree registry: %v", err), 2, pruneJSON,
		)
	}
	expired := reg.ListExpired()
	sort.Slice(expired, func(left, right int) bool {
		return expired[left].Path < expired[right].Path
	})
	report := prunepolicy.Report{
		SchemaVersion: prunepolicy.SchemaVersion,
		Command:       "prune",
		Policy:        "expired",
		DryRun:        pruneDryRun,
		Outcomes:      make([]prunepolicy.Outcome, 0, len(expired)),
	}
	type preparedExpiredCandidate struct {
		entry      *registry.WorktreeEntry
		git        *git.Git
		conditions git.WorktreeRemovalConditions
		outcome    prunepolicy.Outcome
		eligible   bool
	}
	prepared := make([]preparedExpiredCandidate, 0, len(expired))
	progress.Phase("validate candidates", len(expired))
	for index, entry := range expired {
		candidate := preparedExpiredCandidate{
			entry:   entry,
			outcome: prunepolicy.Outcome{Path: entry.Path, Branch: entry.Branch},
		}
		switch {
		case entry.IsMain:
			candidate.outcome.Reason = prunepolicy.MainWorktree
			candidate.outcome.Message = "main worktrees are never eligible for policy removal"
			candidate.outcome.Remediation = "Remove the expiration from the main-worktree registry entry."
		case pathIsMissing(entry.Path):
			candidate.outcome.Reason = prunepolicy.DoctorRequired
			candidate.outcome.Message = "expired registry path is already absent"
			candidate.outcome.Remediation = "Run kwt doctor --fix for structural Git and registry cleanup."
		case git.ValidateWorktreeGeneration(entry.Generation) != nil:
			candidate.outcome.Reason = prunepolicy.MissingGeneration
			candidate.outcome.Message = "live expired worktree has no valid durable generation"
			candidate.outcome.Remediation = "Run kwt list from its repository to initialize Git state, then run kwt doctor --fix to reconcile the registry before retrying."
		default:
			if _, statErr := os.Stat(entry.Path); statErr != nil {
				candidate.outcome.Reason = prunepolicy.PathUnavailable
				candidate.outcome.Message = fmt.Sprintf("could not inspect expired worktree path: %v", statErr)
				candidate.outcome.Remediation = "Restore filesystem access and retry."
				break
			}
			candidate.git = git.New(entry.Path)
			expectedGitDir, gitDirErr := expiredWorktreeGitDir(candidate.git, entry.Path)
			if gitDirErr != nil {
				candidate.outcome = pruneOutcomeForError(entry.Path, entry.Branch, gitDirErr)
				break
			}
			candidate.conditions = git.WorktreeRemovalConditions{
				ExpectedGitDir: expectedGitDir,
				Generation:     entry.Generation,
				RequireClean:   !pruneForce,
			}
			if err := validatePruneExpiredWorktree(candidate.git, entry.Path, candidate.conditions); err != nil {
				candidate.outcome = pruneOutcomeForError(entry.Path, entry.Branch, err)
				break
			}
			if pruneDryRun {
				matched, matchErr := reg.EntryMatches(entry.Path, entry)
				if matchErr != nil {
					candidate.outcome.Reason = prunepolicy.PathUnavailable
					candidate.outcome.Message = fmt.Sprintf("could not revalidate expiration policy: %v", matchErr)
					candidate.outcome.Remediation = "Restore registry access and retry."
					break
				}
				if !matched {
					candidate.outcome = expirationPolicyChangedOutcome(entry)
					break
				}
				candidate.outcome.Reason = prunepolicy.WouldRemove
				candidate.outcome.Message = "expired worktree satisfies removal preconditions"
				break
			}
			candidate.eligible = true
		}
		prepared = append(prepared, candidate)
		progress.Set(index + 1)
	}
	removedWorktrees := 0
	progress.Phase("remove candidates", len(prepared))
	for index, candidate := range prepared {
		entry := candidate.entry
		outcome := candidate.outcome
		if candidate.eligible {
			outcome = func() prunepolicy.Outcome {
				var residualWarning error
				removed, removeErr := removePruneExpiredWorktree(
					candidate.git, entry.Path, pruneForce, candidate.conditions,
					func(remove func() error) (bool, error) {
						return reg.RemoveIfMatchAfter(entry.Path, entry, func() error {
							err := remove()
							if git.WorktreeWasRemoved(err) {
								residualWarning = err
								return nil
							}
							return err
						})
					},
				)
				if removeErr != nil {
					if removed {
						removedWorktrees++
						outcome.Reason = prunepolicy.CleanupIncomplete
						outcome.Message = "worktree was removed but matching registry cleanup did not complete"
						outcome.Remediation = "Run kwt doctor --fix to reconcile the remaining registry state."
						outcome.Evidence = map[string]string{
							"worktree_removed": "true", "cleanup_error": removeErr.Error(),
						}
						return outcome
					}
					return pruneOutcomeForError(entry.Path, entry.Branch, removeErr)
				}
				if !removed {
					return expirationPolicyChangedOutcome(entry)
				}
				removedWorktrees++
				if residualWarning != nil {
					outcome.Reason = prunepolicy.CleanupIncomplete
					outcome.Message = residualWarning.Error()
					outcome.Remediation = "Inspect the path and remove it only if it contains leftovers from the removed worktree."
					outcome.Evidence = map[string]string{
						"worktree_removed": "true", "cleanup_error": residualWarning.Error(),
					}
				} else {
					outcome.Reason = prunepolicy.Removed
					outcome.Message = "expired worktree removed"
				}
				return outcome
			}()
		}
		report.Outcomes = append(report.Outcomes, outcome)
		progress.Set(index + 1)
	}
	report.Finalize()
	progress.Pause()
	if err := renderPruneReport(cmd, report, pruneJSON); err != nil {
		return writeMaintenanceError(
			cmd, "prune", "output_failed", err.Error(), 2, false,
		)
	}
	if removedWorktrees > 0 {
		if cfg, err := loadFleetConfig(); err == nil {
			publishFleetBestEffortForCommand(cmd, cfg)
		}
	}
	if exitCode := report.ExitCode(); exitCode != 0 {
		cmd.Root().SilenceUsage = true
		cmd.Root().SilenceErrors = true
		if !pruneJSON {
			_, _ = fmt.Fprintf(
				cmd.ErrOrStderr(),
				"kwt prune: candidates_skipped: %d candidate(s) require attention\n",
				report.Summary.Skipped,
			)
		}
		return &maintenanceCommandError{
			code: exitCode,
			err:  fmt.Errorf("%d prune candidate(s) require attention", report.Summary.Skipped),
		}
	}
	return nil
}

func expiredWorktreeGitDir(g *git.Git, path string) (string, error) {
	inspections, err := g.InspectWorktrees()
	if err != nil {
		return "", &git.ConditionError{Reason: git.ReasonBacklinkChanged, Path: path}
	}
	matches := make([]git.WorktreeInspection, 0, 1)
	for _, inspection := range inspections {
		if utils.PathKey(inspection.Path) == utils.PathKey(path) {
			matches = append(matches, inspection)
		}
	}
	if len(matches) != 1 {
		return "", &git.ConditionError{Reason: git.ReasonBacklinkChanged, Path: path}
	}
	inspection := matches[0]
	if inspection.GitDirError != "" || inspection.GitDir == "" ||
		inspection.DotGitTarget == "" ||
		utils.PathKey(inspection.DotGitTarget) != utils.PathKey(inspection.GitDir) {
		return "", &git.ConditionError{Reason: git.ReasonBacklinkChanged, Path: path}
	}
	return inspection.GitDir, nil
}

func expirationPolicyChangedOutcome(entry *registry.WorktreeEntry) prunepolicy.Outcome {
	return prunepolicy.Outcome{
		Path: entry.Path, Branch: entry.Branch,
		Reason:      prunepolicy.ExpirationPolicyChanged,
		Message:     "expiration policy changed after candidate inspection",
		Remediation: "Review the current expiration and retry if the worktree is still eligible.",
	}
}

func pathIsMissing(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

func pruneOutcomeForError(path string, branch string, err error) prunepolicy.Outcome {
	outcome := prunepolicy.Outcome{Path: path, Branch: branch}
	var conditionErr *git.ConditionError
	if errors.As(err, &conditionErr) {
		switch conditionErr.Reason {
		case git.ReasonBacklinkChanged:
			outcome.Reason = prunepolicy.DoctorRequired
			outcome.Message = "worktree backlink changed after candidate selection"
			outcome.Remediation = "Run kwt doctor and repair the reported structural state before retrying."
		case git.ReasonGenerationChanged:
			outcome.Reason = prunepolicy.GenerationChanged
			outcome.Message = "worktree generation changed after candidate selection"
		case git.ReasonHeadChanged:
			outcome.Reason = prunepolicy.HeadChanged
			outcome.Message = "worktree HEAD changed after candidate selection"
		case git.ReasonRepositoryChanged:
			outcome.Reason = prunepolicy.RepositoryChanged
			outcome.Message = "worktree repository identity changed after candidate selection"
		case git.ReasonBranchChanged:
			outcome.Reason = prunepolicy.SourceBranchMismatch
			outcome.Message = "worktree branch changed after candidate selection"
		case git.ReasonUpstreamRepositoryChanged:
			outcome.Reason = prunepolicy.SourceRepositoryMismatch
			outcome.Message = "worktree upstream repository changed after candidate selection"
		case git.ReasonUpstreamBranchChanged:
			outcome.Reason = prunepolicy.SourceBranchMismatch
			outcome.Message = "worktree upstream branch changed after candidate selection"
		case git.ReasonDirty:
			outcome.Reason = prunepolicy.DirtyWorktree
			outcome.Message = "worktree has uncommitted changes"
			outcome.Remediation = "Commit or discard the changes, or use --force with --expired."
		case git.ReasonLocked:
			outcome.Reason = prunepolicy.LockedWorktree
			outcome.Message = "worktree is locked"
			outcome.Remediation = "Unlock the worktree with git worktree unlock, then retry."
		case git.ReasonMainWorktree:
			outcome.Reason = prunepolicy.MainWorktree
			outcome.Message = "main worktrees are never eligible for policy removal"
			outcome.Remediation = "Remove the expiration from the main-worktree registry entry."
		}
		return outcome
	}
	outcome.Reason = prunepolicy.RemovalFailed
	outcome.Message = fmt.Sprintf("worktree removal failed: %v", err)
	outcome.Remediation = "Inspect the local Git worktree state and retry."
	return outcome
}

func renderPruneReport(
	cmd *cobra.Command,
	report prunepolicy.Report,
	jsonOutput bool,
) error {
	if jsonOutput {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	if len(report.Outcomes) == 0 {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "No %s worktrees found.\n", report.Policy)
		return err
	}
	type outcomeGroup struct {
		reason      prunepolicy.Reason
		message     string
		remediation string
		paths       []string
	}
	groupsByKey := make(map[string]*outcomeGroup)
	var groupKeys []string
	for _, outcome := range report.Outcomes {
		key := string(outcome.Reason) + "\x00" + outcome.Message + "\x00" + outcome.Remediation
		group := groupsByKey[key]
		if group == nil {
			group = &outcomeGroup{reason: outcome.Reason, message: outcome.Message, remediation: outcome.Remediation}
			groupsByKey[key] = group
			groupKeys = append(groupKeys, key)
		}
		group.paths = append(group.paths, outcome.Path)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		left := groupsByKey[groupKeys[i]]
		right := groupsByKey[groupKeys[j]]
		if left.reason != right.reason {
			return left.reason < right.reason
		}
		return groupKeys[i] < groupKeys[j]
	})
	for _, key := range groupKeys {
		group := groupsByKey[key]
		sort.Slice(group.paths, func(i, j int) bool {
			return utils.PathKey(group.paths[i]) < utils.PathKey(group.paths[j])
		})
		if len(group.paths) == 1 {
			if _, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"[%s] %s: %s\n",
				group.reason,
				group.paths[0],
				group.message,
			); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(
			cmd.OutOrStdout(),
			"[%s] %s\n",
			group.reason,
			group.message,
		); err != nil {
			return err
		} else {
			for _, path := range group.paths {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", path); err != nil {
					return err
				}
			}
		}
		if group.remediation != "" {
			if _, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"  remediation: %s\n",
				group.remediation,
			); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"Candidates: %d, removed: %d, would remove: %d, skipped: %d\n",
		report.Summary.Candidates,
		report.Summary.Removed,
		report.Summary.WouldRemove,
		report.Summary.Skipped,
	)
	return err
}
