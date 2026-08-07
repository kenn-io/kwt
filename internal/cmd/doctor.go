package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/maintenance"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

var (
	doctorFix   bool
	doctorJSON  bool
	doctorQuiet bool
)

type doctorRegistry interface {
	maintenance.RegistryMutator
	List() []*registry.WorktreeEntry
	CreationActive(string) (bool, error)
}

var (
	loadDoctorSnapshot = config.LoadGlobalSnapshot
	openDoctorRegistry = func() (doctorRegistry, error) {
		return registry.New()
	}
	doctorInspect = func(
		ctx context.Context,
		cfg *models.Config,
		registrations []config.ProjectRegistration,
		entries []*registry.WorktreeEntry,
		creationActive func(string) (bool, error),
	) (maintenance.Report, error) {
		inspector := maintenance.NewInspector(cfg, entries, registrations)
		inspector.CreationActive = creationActive
		return inspector.Inspect(ctx)
	}
	doctorApplyFixes = func(
		ctx context.Context,
		report maintenance.Report,
		reg doctorRegistry,
		entries []*registry.WorktreeEntry,
	) error {
		return (&maintenance.Fixer{
			Registry: reg, RegistryEntries: entries, Projects: doctorProjectMutator{},
		}).Fix(ctx, report)
	}
)

type doctorProjectMutator struct{}

func (doctorProjectMutator) CompareAndSwapProject(
	expected config.ProjectRegistration,
	replacement *models.Project,
) (bool, error) {
	return config.CompareAndSwapProject(expected, replacement)
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Inspect worktree consistency without changing it",
	Long: `Inspect project registrations, configured repositories, Git worktree
metadata, filesystem backlinks, and the kwt registry. The default mode is
read-only. Use --fix to apply only uniquely owned structural repairs and
stale-record cleanup. When a project path is confirmed-absent, --fix can
relocate or remove the unchanged registration only when the result is
unambiguous.

Read-only inspection does not initialize a missing generation. Adopt a live
generation-less worktree with kwt list from its verified repository before
requesting automated policy removal. Doctor requires Git 2.31 or newer.`,
	Args: cobra.NoArgs,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := requireConfigInitialization(); err != nil {
			return writeMaintenanceError(
				cmd, "doctor", "initialization_failed", err.Error(), 2, doctorJSON,
			)
		}
		return nil
	},
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.Flags().BoolVar(&doctorFix, "fix", false, "Repair uniquely owned structural findings")
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "Output a machine-readable report")
	doctorCmd.Flags().BoolVar(&doctorQuiet, "quiet", false, "Suppress the report; exit status still indicates remaining findings")
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	if doctorQuiet && doctorJSON {
		return writeMaintenanceError(
			cmd, "doctor", "incompatible_flags", "--quiet and --json are mutually exclusive", 2, false,
		)
	}
	if err := checkMaintenanceGitVersion(cmd, "doctor", doctorJSON); err != nil {
		return err
	}
	ctx := cmd.Context()
	snapshot, err := loadDoctorSnapshot()
	if err != nil {
		return writeMaintenanceError(
			cmd, "doctor", "inspection_failed",
			fmt.Sprintf("load global configuration: %v", err), 2, doctorJSON,
		)
	}
	reg, err := openDoctorRegistry()
	if err != nil {
		return writeMaintenanceError(
			cmd, "doctor", "inspection_failed",
			fmt.Sprintf("open worktree registry: %v", err), 2, doctorJSON,
		)
	}
	entries := reg.List()
	report, err := doctorInspect(ctx, snapshot.Config, snapshot.Projects, entries, reg.CreationActive)
	if err != nil {
		return writeMaintenanceError(
			cmd, "doctor", "inspection_failed", err.Error(), 2, doctorJSON,
		)
	}
	beforeFix := report
	if doctorFix && report.Summary.Findings > 0 {
		if err := doctorApplyFixes(ctx, report, reg, entries); err != nil {
			return writeMaintenanceError(
				cmd, "doctor", "fix_failed", err.Error(), 2, doctorJSON,
			)
		}
		snapshot, err = loadDoctorSnapshot()
		if err != nil {
			return writeMaintenanceError(
				cmd, "doctor", "inspection_failed",
				fmt.Sprintf("reload global configuration after fixes: %v", err), 2, doctorJSON,
			)
		}
		reg, err = openDoctorRegistry()
		if err != nil {
			return writeMaintenanceError(
				cmd, "doctor", "inspection_failed",
				fmt.Sprintf("reopen worktree registry after fixes: %v", err), 2, doctorJSON,
			)
		}
		report, err = doctorInspect(ctx, snapshot.Config, snapshot.Projects, reg.List(), reg.CreationActive)
		if err != nil {
			return writeMaintenanceError(
				cmd, "doctor", "inspection_failed",
				fmt.Sprintf("rescan after fixes: %v", err), 2, doctorJSON,
			)
		}
		report.Fixed = resolvedDoctorFindings(beforeFix, report)
		for _, repository := range report.Fixed {
			report.Summary.FixedFindings += len(repository.Findings)
		}
	}
	report.SchemaVersion = maintenance.SchemaVersion
	report.Command = "doctor"
	report.Fix = doctorFix
	if !doctorQuiet {
		if err := renderDoctorReport(cmd, report, doctorJSON); err != nil {
			return writeMaintenanceError(
				cmd, "doctor", "output_failed", err.Error(), 2, doctorJSON,
			)
		}
	}
	if report.Summary.Findings > 0 {
		cmd.Root().SilenceUsage = true
		cmd.Root().SilenceErrors = true
		return &maintenanceCommandError{
			code: 1,
			err:  fmt.Errorf("%d worktree maintenance issue(s) remain", report.Summary.Findings),
		}
	}
	return nil
}

func resolvedDoctorFindings(before, after maintenance.Report) []maintenance.RepositoryReport {
	type findingKey struct {
		code maintenance.FindingCode
		path string
	}
	remaining := make(map[findingKey]bool)
	for _, repository := range after.Repositories {
		for _, finding := range repository.Findings {
			remaining[findingKey{code: finding.Code, path: utils.PathKey(finding.Path)}] = true
		}
	}
	resolved := make([]maintenance.RepositoryReport, 0)
	for _, repository := range before.Repositories {
		fixed := repository
		fixed.Worktrees = nil
		fixed.Findings = nil
		for _, finding := range repository.Findings {
			key := findingKey{code: finding.Code, path: utils.PathKey(finding.Path)}
			if finding.Fixable && !remaining[key] {
				fixed.Findings = append(fixed.Findings, finding)
			}
		}
		if len(fixed.Findings) > 0 {
			resolved = append(resolved, fixed)
		}
	}
	return resolved
}
