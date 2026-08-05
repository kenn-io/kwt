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
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/table"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

var (
	projectsJSON       bool
	projectsAddJSON    bool
	loadProjectsConfig = config.Load
	registerProject    = config.RegisterProject
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
			return writeProjectCommandError(
				cmd,
				"registration_failed",
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

func init() {
	rootCmd.AddCommand(projectsCmd)
	projectsCmd.Flags().BoolVar(&projectsJSON, "json", false, "Output in JSON format")
	projectsAddCmd.Flags().BoolVar(&projectsAddJSON, "json", false, "Output a machine-readable result")
	projectsCmd.AddCommand(projectsAddCmd)
	projectsCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writeProjectCommandError(cmd, "invalid_repository", err.Error(), 2)
	})
}

func runProjects(cmd *cobra.Command, args []string) error {
	cfg, err := loadProjectsConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	projects := canonicalizeProjectIdentities(cfg.Projects)

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

type projectAddResult struct {
	Status  string         `json:"status"`
	Project models.Project `json:"project"`
}

type projectCommandErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type projectCommandErrorEnvelope struct {
	Error projectCommandErrorBody `json:"error"`
}

type projectCommandError struct {
	body     projectCommandErrorBody
	exitCode int
}

func (e *projectCommandError) Error() string { return e.body.Message }
func (e *projectCommandError) ExitCode() int { return e.exitCode }

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
	if err := registerProject(project); err != nil {
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
		return encoder.Encode(projectAddResult{
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
	body := projectCommandErrorBody{
		Code:      code,
		Message:   message,
		Retryable: false,
	}
	cmd.Root().SilenceUsage = true
	cmd.Root().SilenceErrors = true
	if projectCommandJSONRequested() {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(projectCommandErrorEnvelope{Error: body})
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "kwt projects: %s: %s\n", code, message)
	return &projectCommandError{body: body, exitCode: exitCode}
}

func projectCommandJSONRequested() bool {
	if projectsJSON || projectsAddJSON {
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

// canonicalizeProjectIdentities returns accessible registered projects with
// Repository values resolved through the canonical identity bar, so projects
// output (JSON and table) emits the same identities kwt list --json reports.
func canonicalizeProjectIdentities(projects []models.Project) []models.Project {
	out := make([]models.Project, 0, len(projects))
	for _, project := range projects {
		if strings.TrimSpace(project.Path) == "" {
			continue
		}
		repositoryGit := worktree.NewCachedIdentityGit(git.New(project.Path))
		mainPath, err := repositoryGit.GetMainRepositoryPath()
		if err != nil || !samePath(mainPath, project.Path) {
			continue
		}
		info, err := worktree.RepositoryInfoWithProjects(
			repositoryGit,
			[]models.Project{project},
		)
		if err != nil {
			continue
		}
		project.Repository = info.FullPath
		out = append(out, project)
	}
	return out
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
