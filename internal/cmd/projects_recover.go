package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/maintenance"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/service"
)

type projectRecoveryResult struct {
	Status  string      `json:"status"`
	Project kwt.Project `json:"project"`
}

func init() {
	projectsCmd.AddCommand(newProjectsRecoverCommand())
}

func newProjectsRecoverCommand() *cobra.Command {
	var destination, repository, fingerprint string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "recover <path>",
		Short: "Relocate a missing project without removing registrations or worktrees",
		Long: "Recover one missing project using the repositories already known to kwt. " +
			"Use --to to choose its new checkout. Unmatched and ambiguous projects remain registered. " +
			"JSON callers must supply the observed repository identity and registration fingerprint.",
		Args: projectsExactArgs(1),
		RunE: withGracefulSignals(func(cmd *cobra.Command, args []string) error {
			result, err := recoverProject(cmd.Context(), args[0], destination, repository, fingerprint, jsonOutput)
			if err != nil {
				return writeProjectServiceError(cmd, service.AsError(err), jsonOutput)
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s at %s\n", result.Status, result.Project.Name, result.Project.Path)
			return err
		}),
	}
	command.Flags().StringVar(&destination, "to", "", "New main checkout path")
	command.Flags().StringVar(&repository, "expected-repository", "", "Observed repository identity")
	command.Flags().StringVar(&fingerprint, "expected-registration", "", "Observed registration fingerprint")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Output a machine-readable result")
	return command
}

func recoverProject(
	ctx context.Context, path, destination, identity, fingerprint string, requireExpectation bool,
) (projectRecoveryResult, error) {
	var result projectRecoveryResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if (identity == "") != (fingerprint == "") ||
		(requireExpectation && identity == "") ||
		(fingerprint != "" && !config.ValidProjectRegistrationFingerprint(fingerprint)) {
		return result, service.NewError(service.InvalidRequest,
			"expected repository and registration fingerprint are required together", false, nil, nil)
	}
	if identity != "" {
		if _, err := lifecycle.ValidateProjectIdentity(identity); err != nil {
			return result, service.NewError(service.InvalidRequest,
				"expected repository identity is invalid", false, nil, err)
		}
	}
	snapshot, err := config.LoadGlobalSnapshot()
	if err != nil {
		return result, err
	}
	var matches []config.ProjectRegistration
	for _, registration := range snapshot.Projects {
		if registration.Persisted.Path == path {
			matches = append(matches, registration)
		}
	}
	if len(matches) != 1 {
		return result, service.NewError(service.ProjectNotFound,
			"one unchanged project must be registered at the exact path", false, nil, nil)
	}
	expected := matches[0]
	project, err := recoveryProjectRecord(ctx, expected)
	if err != nil {
		return result, err
	}
	if identity != "" && (!lifecycle.EqualProjectIdentity(identity, project.Repository) ||
		fingerprint != project.RegistrationFingerprint) {
		return result, service.NewError(service.RegistrationChanged,
			"the project registration changed; refresh before locating it", true, nil, nil)
	}
	result = projectRecoveryResult{Status: "unresolved", Project: project}
	if project.PathIssue != "missing" {
		if project.PathIssue == "" {
			result.Status = "available"
		}
		return result, nil
	}

	inspector := maintenance.NewInspector(snapshot.Config, nil, snapshot.Projects)
	var finding maintenance.Finding
	if destination != "" {
		target, inspectErr := inspector.InspectRepository(destination)
		if inspectErr != nil || !lifecycle.EqualProjectIdentity(target.RepositoryIdentity, project.Repository) ||
			utils.PathKey(target.Root) != utils.PathKey(destination) {
			return result, service.NewError(service.InvalidRequest,
				"choose the main checkout of the same repository", false, nil, inspectErr)
		}
		finding = maintenance.Finding{
			Code: maintenance.ProjectPathMoved, Path: expected.Effective.Path, Fixable: true,
			ProjectRepair: &maintenance.ProjectRepairCondition{
				Action: maintenance.RelocateProject, Expected: expected,
				TargetRoot: target.Root, TargetCommonDir: target.CommonDir,
				TargetRepository: target.RepositoryIdentity,
			},
		}
	} else {
		reg, openErr := openDoctorRegistry()
		if openErr != nil {
			return result, recoveryInventoryError(ctx, openErr)
		}
		inspector.RegistryEntries = reg.List()
		inspector.CreationActive = reg.CreationActive
		report, inspectErr := inspector.Inspect(ctx)
		if inspectErr != nil {
			return result, recoveryInventoryError(ctx, inspectErr)
		}
		for _, repository := range report.Repositories {
			for _, candidate := range repository.Findings {
				if candidate.Fixable && candidate.ProjectRepair != nil &&
					candidate.ProjectRepair.Action == maintenance.RelocateProject &&
					candidate.ProjectRepair.Expected.SamePersistedEntry(expected) {
					finding = candidate
				}
			}
		}
	}
	if finding.ProjectRepair == nil {
		return result, nil
	}
	replacement := expected.Persisted
	replacement.Path = finding.ProjectRepair.TargetRoot
	replacement.Repository = finding.ProjectRepair.TargetRepository
	replacementFingerprint, err := expected.ReplacementFingerprint(replacement)
	if err != nil {
		return result, err
	}
	// Only this relocation enters the fixer. Other doctor findings, including
	// stale registrations and missing worktrees, are outside this command.
	changed, err := (&maintenance.Fixer{Projects: doctorProjectMutator{}}).FixProject(ctx, finding)
	if err != nil {
		return result, err
	}
	if !changed {
		return result, service.NewError(service.RegistrationChanged,
			"the project or its destination changed; refresh before locating it", true, nil, nil)
	}
	current, err := config.LoadGlobalSnapshot()
	if err != nil {
		return result, err
	}
	for _, registration := range current.Projects {
		if registration.Persisted.Path == path {
			return result, service.NewError(service.RegistrationChanged,
				"the project or its destination changed; refresh before locating it", true, nil, nil)
		}
	}
	for _, registration := range current.Projects {
		if utils.PathKey(registration.Effective.Path) == utils.PathKey(finding.ProjectRepair.TargetRoot) {
			updated, recordErr := recoveryProjectRecord(ctx, registration)
			if recordErr != nil {
				return result, recordErr
			}
			if updated.PathIssue == "" && updated.RegistrationFingerprint == replacementFingerprint &&
				lifecycle.EqualProjectIdentity(updated.Repository, project.Repository) {
				return projectRecoveryResult{Status: "recovered", Project: updated}, nil
			}
		}
	}
	return result, service.NewError(service.RegistrationChanged,
		"the project or its destination changed; refresh before locating it", true, nil, nil)
}

// Automatic recovery needs a complete inventory before relocating anything.
// Filesystem access failures leave the project unresolved; malformed data and
// command failures still need an error, and cancellation must not become success.
func recoveryInventoryError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if _, ok := errors.AsType[*fs.PathError](err); ok {
		return nil
	}
	return err
}

func recoveryProjectRecord(ctx context.Context, registration config.ProjectRegistration) (kwt.Project, error) {
	identity, err := resolveDoctorProjectIdentity(ctx, registration)
	if err != nil {
		return kwt.Project{}, err
	}
	fingerprint, err := registration.Fingerprint()
	if err != nil {
		return kwt.Project{}, err
	}
	return kwt.Project{
		Repository: identity, Name: registration.Persisted.Name,
		Path: registration.Persisted.Path, LastTouched: registration.Persisted.LastTouched,
		RegistrationFingerprint: fingerprint,
		PathIssue:               lifecycle.ProjectPathIssue(registration.Effective.Path),
	}, nil
}
