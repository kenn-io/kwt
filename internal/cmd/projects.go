package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/table"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

var (
	projectsJSON                 bool
	projectsAddJSON              bool
	projectsRemoveJSON           bool
	loadProjectsConfig           = config.Load
	registerProject              = registerProjectWithLifecycle
	queryProjectsInventory       = queryCLIInventory
	removeProjectThroughDaemon   = removeProjectViaDaemon
	projectsExpectedRepository   string
	projectsExpectedRegistration string
)

var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "List registered project repositories",
	Long: `List repositories kwt has registered for cross-project discovery.

Registered projects hold main-repository paths that may live outside the
configured worktree base directory. Use --json for a machine-readable surface
that external automation can consume without parsing the config file.`,
	Args: projectsNoArgs,
	// Isolation: projects is a global registry surface and must not merge the
	// caller's cwd .kwt.toml. The command still propagates global config
	// initialization failures through Cobra.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := requireConfigInitialization(); err != nil {
			code := "registration_failed"
			if cmd == projectsRemoveCmd {
				code = "unregistration_failed"
			}
			return writeProjectCommandError(
				cmd,
				code,
				fmt.Sprintf("failed to initialize configuration: %v", err),
				1,
			)
		}
		return nil
	},
	RunE: runProjects,
}

var projectsAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Register an existing Git repository",
	Args:  projectsExactArgs(1),
	RunE:  runProjectsAdd,
}

var projectsRemoveCmd = &cobra.Command{
	Use:   "remove <path>",
	Short: "Unregister a project without deleting its repository",
	Args:  projectsExactArgs(1),
	RunE:  withGracefulSignals(runProjectsRemove),
}

func init() {
	rootCmd.AddCommand(projectsCmd)
	projectsCmd.Flags().BoolVar(&projectsJSON, "json", false, "Output in JSON format")
	projectsAddCmd.Flags().BoolVar(&projectsAddJSON, "json", false, "Output a machine-readable result")
	projectsRemoveCmd.Flags().BoolVar(&projectsRemoveJSON, "json", false, "Output a machine-readable result")
	projectsRemoveCmd.Flags().StringVar(
		&projectsExpectedRepository,
		"expected-repository",
		"",
		"Require the exact credential-free repository identity",
	)
	projectsRemoveCmd.Flags().StringVar(
		&projectsExpectedRegistration,
		"expected-registration",
		"",
		"Require the exact observed registration fingerprint",
	)
	projectsCmd.AddCommand(projectsAddCmd, projectsRemoveCmd)
	projectsCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writeProjectCommandError(cmd, "invalid_repository", err.Error(), 2)
	})
}

func runProjects(cmd *cobra.Command, args []string) error {
	result, err := queryProjectsInventory(
		cmd.Context(),
		kwt.Request{View: kwt.ViewProjects, RequireCurrent: true},
		false,
		cmd.ErrOrStderr(),
	)
	if err != nil {
		if projectsJSON {
			return writeCommandFailure(
				cmd,
				service.AsError(err).Descriptor,
				1,
				true,
				"projects",
			)
		}
		return err
	}
	projects := result.Snapshot.Projects

	if projectsJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(projects)
	}

	if len(projects) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no projects registered")
		return nil
	}

	t := table.New().SetOutput(cmd.OutOrStdout()).Headers("NAME", "REPOSITORY", "PATH", "LAST TOUCHED")
	for _, project := range projects {
		t.Row(project.Name, project.Repository, project.Path, project.LastTouched)
	}
	return t.Println()
}

type projectMutationResult struct {
	Status  string         `json:"status"`
	Project models.Project `json:"project"`
}

func projectsNoArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return writeProjectCommandError(
		cmd,
		"invalid_repository",
		"this command does not accept positional arguments",
		2,
	)
}

func projectsExactArgs(count int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == count {
			return nil
		}
		return writeProjectCommandError(
			cmd,
			"invalid_repository",
			fmt.Sprintf("expected %d repository path, received %d", count, len(args)),
			2,
		)
	}
}

func runProjectsAdd(cmd *cobra.Command, args []string) error {
	project, err := resolveProjectForRegistration(args[0])
	if err != nil {
		return writeProjectCommandError(
			cmd,
			"invalid_repository",
			err.Error(),
			2,
		)
	}
	cfg, err := loadProjectsConfig()
	if err != nil {
		return writeProjectCommandError(
			cmd,
			"registration_failed",
			fmt.Sprintf("failed to load projects: %v", err),
			1,
		)
	}
	for _, existing := range cfg.Projects {
		if !samePath(existing.Path, project.Path) {
			continue
		}
		if reusable, ok := reusableExistingProject(existing, project); ok {
			project = reusable
		}
		break
	}
	project.LastTouched = time.Now().UTC().Format(time.RFC3339)
	if err := registerProject(cmd.Context(), project); err != nil {
		return writeProjectCommandError(
			cmd,
			"registration_failed",
			fmt.Sprintf("failed to register project: %v", err),
			1,
		)
	}

	if projectsAddJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(projectMutationResult{
			Status:  "registered",
			Project: project,
		})
	}
	_, err = fmt.Fprintf(
		cmd.OutOrStdout(),
		"registered project %s at %s\n",
		project.Name,
		project.Path,
	)
	return err
}

func runProjectsRemove(cmd *cobra.Command, args []string) error {
	expectedRepository := projectsExpectedRepository
	expectedRegistration := projectsExpectedRegistration
	hasRepository := expectedRepository != ""
	hasRegistration := expectedRegistration != ""
	if hasRepository != hasRegistration || (projectsRemoveJSON && !hasRepository) {
		return writeProjectServiceError(cmd, service.NewError(
			service.InvalidRequest,
			"expected repository identity and registration fingerprint are required together",
			false, nil, nil,
		))
	}
	if !hasRepository {
		var err error
		expectedRepository, expectedRegistration, err = currentProjectRemovalExpectation(
			cmd, args[0],
		)
		if err != nil {
			return writeProjectServiceError(cmd, service.AsError(err))
		}
	}
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return writeProjectServiceError(cmd, service.NewError(
			service.UnregistrationFailed,
			"failed to capture project removal context",
			false, nil, err,
		))
	}
	result, err := removeProjectThroughDaemon(cmd.Context(), kwt.ProjectRemovalRequest{
		Path: args[0], ExpectedRepository: expectedRepository,
		ExpectedRegistration: expectedRegistration, Expansion: expansion,
	})
	if err != nil {
		return writeProjectServiceError(cmd, service.AsError(err))
	}
	project := result.Project
	if projectsRemoveJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(projectMutationResult{
			Status:  "unregistered",
			Project: project,
		})
	}
	_, err = fmt.Fprintf(
		cmd.OutOrStdout(),
		"unregistered project %s at %s\n",
		project.Name,
		project.Path,
	)
	return err
}

func currentProjectRemovalExpectation(
	cmd *cobra.Command,
	path string,
) (string, string, error) {
	result, err := queryProjectsInventory(
		cmd.Context(),
		kwt.Request{View: kwt.ViewProjects, RequireCurrent: true},
		false,
		cmd.ErrOrStderr(),
	)
	if err != nil {
		return "", "", err
	}
	matches := make([]kwt.Project, 0, 1)
	for _, project := range result.Snapshot.Projects {
		if project.Path == path {
			matches = append(matches, project)
		}
	}
	switch len(matches) {
	case 0:
		return "", "", service.NewError(
			service.ProjectNotFound,
			"no project is registered at the exact path",
			false, nil, nil,
		)
	case 1:
		if matches[0].RegistrationFingerprint == "" {
			return "", "", service.NewError(
				service.Internal,
				"internal failure",
				false, nil, nil,
			)
		}
		return matches[0].Repository, matches[0].RegistrationFingerprint, nil
	default:
		return "", "", service.NewError(
			service.UnregistrationFailed,
			"multiple project registrations use the exact path",
			false, nil, nil,
		)
	}
}

func writeProjectServiceError(cmd *cobra.Command, typed *service.Error) error {
	exitCode := 1
	if typed.Code == service.InvalidRequest || typed.Code == service.ProjectNotFound {
		exitCode = 2
	}
	return writeCommandFailure(
		cmd,
		typed.Descriptor,
		exitCode,
		projectCommandJSONRequested(),
		"projects",
	)
}

func resolveProjectForRegistration(path string) (models.Project, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return models.Project{}, fmt.Errorf("repository path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return models.Project{}, fmt.Errorf(
			"resolve repository path %q: %w",
			path,
			err,
		)
	}
	if resolved, err := filepath.EvalSymlinks(absolutePath); err == nil {
		absolutePath = resolved
	}
	repositoryGit := git.New(absolutePath)
	mainPath, err := repositoryGit.GetMainRepositoryPath()
	if err != nil {
		return models.Project{}, fmt.Errorf(
			"%s is not an accessible Git repository",
			absolutePath,
		)
	}
	if resolved, err := filepath.EvalSymlinks(mainPath); err == nil {
		mainPath = resolved
	}
	info, err := worktree.RepositoryInfoFromGit(repositoryGit)
	if err != nil {
		return models.Project{}, fmt.Errorf(
			"resolve repository identity for %s: %w",
			absolutePath,
			err,
		)
	}
	name := info.Repository
	if name == "" {
		name = filepath.Base(mainPath)
	}
	return models.Project{
		Repository: info.FullPath,
		Name:       name,
		Path:       mainPath,
	}, nil
}

func writeProjectCommandError(
	cmd *cobra.Command,
	code string,
	message string,
	exitCode int,
) error {
	return writeProjectCommandErrorWithRetry(
		cmd, code, message, exitCode, false,
	)
}

func writeProjectCommandErrorWithRetry(
	cmd *cobra.Command,
	code string,
	message string,
	exitCode int,
	retryable bool,
) error {
	return writeCommandFailure(
		cmd,
		service.Descriptor{
			Code: service.Code(code), Message: message, Retryable: retryable,
		},
		exitCode,
		projectCommandJSONRequested(),
		"projects",
	)
}

func projectCommandJSONRequested() bool {
	if projectsJSON || projectsAddJSON || projectsRemoveJSON {
		return true
	}
	// pflag stops at the first parse error, so a later --json never reaches
	// the bound variable. Preserve the caller's output choice from raw argv.
	requested := false
	for _, argument := range os.Args[1:] {
		if argument == "--" {
			break
		}
		if argument == "--json" {
			requested = true
			continue
		}
		name, value, ok := strings.Cut(argument, "=")
		if !ok || name != "--json" {
			continue
		}
		enabled, err := strconv.ParseBool(value)
		if err == nil {
			requested = enabled
		}
	}
	return requested
}

// publishableProjectRepository resolves the repository identity projects
// emits for a registry entry: path-backed entries resolve through the same
// registered-identity resolver every other surface uses; otherwise a stored
// canonical identity is authoritative, and path fallbacks resolve through the
// canonical local-path identity. A raw unvalidated registry value is never
// emitted.
func publishableProjectRepository(project models.Project) string {
	if project.Path != "" {
		// The same registered-identity precedence kwt list uses, applied to
		// this single entry.
		info, err := worktree.RepositoryInfoWithProjects(
			git.New(project.Path), []models.Project{project})
		if err == nil {
			return info.FullPath
		}
	}
	if identity, ok := url.CanonicalRepositoryIdentity(project.Repository); ok {
		return identity
	}
	if project.Path != "" {
		if info, err := worktree.RepositoryInfoFromLocalPath(project.Path); err == nil {
			return info.FullPath
		}
	}
	if stored := strings.TrimSpace(project.Repository); url.IsLocalFallbackIdentity(stored) {
		return stored
	}
	return ""
}
