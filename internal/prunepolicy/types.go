// Package prunepolicy defines stable, provider-neutral prune outcomes.
package prunepolicy

const SchemaVersion = 2

type Reason string

const (
	Removed                     Reason = "removed"
	WouldRemove                 Reason = "would_remove"
	MissingGeneration           Reason = "missing_generation"
	DoctorRequired              Reason = "doctor_required"
	DirtyWorktree               Reason = "dirty_worktree"
	EligibleMerged              Reason = "eligible_merged"
	HeadAdvancedAfterPR         Reason = "head_advanced_after_pr"
	NoAssociatedPR              Reason = "no_associated_pr"
	PRNotMerged                 Reason = "pr_not_merged"
	SourceRepositoryUnavailable Reason = "source_repository_unavailable"
	SourceRepositoryMismatch    Reason = "source_repository_mismatch"
	SourceBranchMismatch        Reason = "source_branch_mismatch"
	AmbiguousPRMatch            Reason = "ambiguous_pr_match"
	AuthenticationFailed        Reason = "authentication_failed"
	NetworkFailure              Reason = "network_failure"
	ProviderFailure             Reason = "provider_failure"
	GenerationChanged           Reason = "generation_changed"
	ExpirationPolicyChanged     Reason = "expiration_policy_changed"
	HeadChanged                 Reason = "head_changed"
	RepositoryChanged           Reason = "repository_identity_changed"
	MainWorktree                Reason = "main_worktree"
	LockedWorktree              Reason = "locked_worktree"
	PathUnavailable             Reason = "path_unavailable"
	RemovalFailed               Reason = "removal_failed"
	CleanupIncomplete           Reason = "cleanup_incomplete"
	WouldRequireConfirmation    Reason = "would_require_confirmation"
	ConfirmationRequired        Reason = "confirmation_required"
	ConfirmationDeclined        Reason = "confirmation_declined"
	ProtectedSessionLive        Reason = "protected_session_live"
	ProtectedEndpointIncomplete Reason = "protected_endpoint_inventory_incomplete"
	RegistrationChanged         Reason = "registration_changed"
)

type Outcome struct {
	Path        string            `json:"path"`
	Branch      string            `json:"branch,omitempty"`
	Reason      Reason            `json:"reason"`
	Message     string            `json:"message"`
	Remediation string            `json:"remediation,omitempty"`
	Evidence    map[string]string `json:"evidence,omitempty"`
}

type Summary struct {
	Candidates  int `json:"candidates"`
	Removed     int `json:"removed"`
	WouldRemove int `json:"would_remove"`
	Skipped     int `json:"skipped"`
}

type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Command       string    `json:"command"`
	Policy        string    `json:"policy"`
	DryRun        bool      `json:"dry_run"`
	Outcomes      []Outcome `json:"outcomes"`
	Summary       Summary   `json:"summary"`
}

// Finalize derives summary counts from outcomes.
func (r *Report) Finalize() {
	r.Summary = Summary{Candidates: len(r.Outcomes)}
	for _, outcome := range r.Outcomes {
		switch outcome.Reason {
		case Removed:
			r.Summary.Removed++
		case WouldRemove, WouldRequireConfirmation:
			r.Summary.WouldRemove++
		default:
			r.Summary.Skipped++
		}
	}
}

// ExitCode returns the candidate-level status. Global inspection and usage
// errors are classified by the command before a report exists.
func (r Report) ExitCode() int {
	for _, outcome := range r.Outcomes {
		switch outcome.Reason {
		case Removed, WouldRemove, WouldRequireConfirmation, ConfirmationDeclined, NoAssociatedPR, PRNotMerged:
			continue
		default:
			return 1
		}
	}
	return 0
}
