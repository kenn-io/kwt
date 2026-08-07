// Package maintenance inspects and repairs local worktree consistency without
// consulting pull-request providers.
package maintenance

import (
	"go.kenn.io/kwt/internal/config"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
)

const SchemaVersion = 1

type FindingCode string

const (
	BrokenWorktreeBacklink     FindingCode = "broken_worktree_backlink"
	AmbiguousWorktreeBacklink  FindingCode = "ambiguous_worktree_backlink"
	MissingWorktreeDirectory   FindingCode = "missing_worktree_directory"
	StaleRegistryEntry         FindingCode = "stale_registry_entry"
	UnverifiedRegistryEntry    FindingCode = "unverified_registry_entry"
	MissingGeneration          FindingCode = "missing_generation"
	RegistryGenerationMismatch FindingCode = "registry_generation_mismatch"
	DuplicateRegistryEntry     FindingCode = "duplicate_registry_entry"
	ProjectUnreachable         FindingCode = "project_unreachable"
	RepositoryIdentityMismatch FindingCode = "repository_identity_mismatch"
	ProjectPathMoved           FindingCode = "project_path_moved"
	StaleProjectRegistration   FindingCode = "stale_project_registration"
	AmbiguousProjectRelocation FindingCode = "ambiguous_project_relocation"
)

type ProjectRepairAction string

const (
	RelocateProject ProjectRepairAction = "relocate"
	RemoveProject   ProjectRepairAction = "remove"
)

// ProjectRepairCondition carries the raw compare-and-swap token and the live
// target facts that the fixer must revalidate. It is deliberately excluded
// from reports because the persisted project may contain sensitive values.
type ProjectRepairCondition struct {
	Action           ProjectRepairAction
	Expected         config.ProjectRegistration
	TargetRoot       string
	TargetCommonDir  string
	TargetRepository string
}

// RegistryAliasRepairCondition carries the complete observed alias group for
// one atomic registry compare-and-swap. A nil Retained entry removes a
// confirmed-absent group. It is excluded from serialized reports.
type RegistryAliasRepairCondition struct {
	Expected []*registry.WorktreeEntry
	Retained *registry.WorktreeEntry
}

type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type Finding struct {
	Code                FindingCode                   `json:"code"`
	Severity            Severity                      `json:"severity"`
	Path                string                        `json:"path,omitempty"`
	Message             string                        `json:"message"`
	Remediation         string                        `json:"remediation"`
	Fixable             bool                          `json:"fixable"`
	Evidence            map[string]string             `json:"evidence,omitempty"`
	ProjectRepair       *ProjectRepairCondition       `json:"-"`
	RegistryAliasRepair *RegistryAliasRepairCondition `json:"-"`
}

type RepositoryReport struct {
	Root               string                          `json:"root,omitempty"`
	CommonDir          string                          `json:"common_dir,omitempty"`
	RepositoryIdentity string                          `json:"repository_identity,omitempty"`
	ProjectNames       []string                        `json:"project_names,omitempty"`
	Worktrees          []gitadapter.WorktreeInspection `json:"worktrees,omitempty"`
	Findings           []Finding                       `json:"findings"`
}

type Summary struct {
	Healthy         bool `json:"healthy"`
	Repositories    int  `json:"repositories"`
	Findings        int  `json:"findings"`
	FixableFindings int  `json:"fixable_findings"`
	ManualFindings  int  `json:"manual_findings"`
	FixedFindings   int  `json:"fixed_findings,omitempty"`
}

type Report struct {
	SchemaVersion int                `json:"schema_version"`
	Command       string             `json:"command"`
	Fix           bool               `json:"fix"`
	Fixed         []RepositoryReport `json:"fixed,omitempty"`
	Repositories  []RepositoryReport `json:"repositories"`
	Summary       Summary            `json:"summary"`
}

// RepositorySnapshot is the provider-free local state for one Git common
// directory before findings are classified.
type RepositorySnapshot struct {
	Root                     string
	CommonDir                string
	RepositoryIdentity       string
	LiveRepositoryIdentities map[string]string
	Worktrees                []gitadapter.WorktreeInspection
}
