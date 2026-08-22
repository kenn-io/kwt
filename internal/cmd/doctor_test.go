package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/maintenance"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/pkg/models"
)

func TestDoctorProjectRemovalUsesGuardedLifecycleService(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("KWT_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\npath = '/repo '\nrepository = 'github.com/acme/widget'\n",
	), 0o600))
	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	expected := snapshot.Projects[0]
	oldResolver := resolveDoctorProjectIdentity
	oldRemover := removeDoctorProjectRegistration
	t.Cleanup(func() {
		resolveDoctorProjectIdentity = oldResolver
		removeDoctorProjectRegistration = oldRemover
	})
	resolveDoctorProjectIdentity = func(context.Context, config.ProjectRegistration) (string, error) {
		return "github.com/acme/widget", nil
	}
	var request kwt.ProjectRemovalRequest
	removeDoctorProjectRegistration = func(
		_ context.Context,
		gotHome string,
		got kwt.ProjectRemovalRequest,
		gotExpected config.ProjectRegistration,
	) (bool, error) {
		assert.Equal(t, home, gotHome)
		assert.True(t, gotExpected.SamePersistedEntry(expected))
		request = got
		return true, nil
	}

	changed, err := (doctorProjectMutator{}).RemoveProject(context.Background(), expected)

	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "/repo ", request.Path)
	assert.Equal(t, "github.com/acme/widget", request.ExpectedRepository)
	fingerprint, err := expected.Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, fingerprint, request.ExpectedRegistration)
}

func TestDoctorProjectRemovalPreservesChangedRegistration(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("KWT_HOME", home)
	configPath := filepath.Join(home, "config.toml")
	path := filepath.Join(t.TempDir(), "widget")
	require.NoError(t, os.WriteFile(configPath, []byte(
		"[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'before'\npath = '"+path+"'\n",
	), 0o600))
	snapshot, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	expected := snapshot.Projects[0]
	require.NoError(t, os.WriteFile(configPath, []byte(
		"[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'after'\npath = '"+path+"'\n",
	), 0o600))

	changed, err := (doctorProjectMutator{}).RemoveProject(
		context.Background(), expected,
	)

	require.NoError(t, err)
	assert.False(t, changed)
	current, err := config.LoadGlobalSnapshotAt(home)
	require.NoError(t, err)
	require.Len(t, current.Projects, 1)
	assert.Equal(t, "after", current.Projects[0].Persisted.Name)
}

func TestDoctorProjectRelocationUsesLifecycleTransition(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("KWT_HOME", home)
	expected := config.ProjectRegistration{Persisted: models.Project{
		Path: "/old/widget", Repository: "github.com/acme/widget",
	}}
	replacement := models.Project{
		Path: "/new/widget", Repository: "github.com/acme/widget",
	}
	oldTransition := transitionDoctorProjectRegistration
	oldCAS := compareAndSwapDoctorProject
	t.Cleanup(func() {
		transitionDoctorProjectRegistration = oldTransition
		compareAndSwapDoctorProject = oldCAS
	})
	var mutation func() error
	transitionDoctorProjectRegistration = func(
		_ context.Context,
		gotHome string,
		_ kwt.ExpansionContext,
		got models.Project,
		gotMutation func() error,
	) error {
		assert.Equal(t, home, gotHome)
		assert.Equal(t, replacement, got)
		mutation = gotMutation
		return gotMutation()
	}
	compareAndSwapDoctorProject = func(
		gotHome string,
		got config.ProjectRegistration,
		gotReplacement *models.Project,
	) (bool, error) {
		assert.Equal(t, home, gotHome)
		assert.Equal(t, expected, got)
		require.NotNil(t, gotReplacement)
		assert.Equal(t, replacement, *gotReplacement)
		return true, nil
	}

	changed, err := (doctorProjectMutator{}).RelocateProject(
		context.Background(), expected, replacement,
	)

	require.NoError(t, err)
	assert.True(t, changed)
	require.NotNil(t, mutation)
}

func TestDoctorReadOnlyReportsFixInstruction(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
		return doctorFindingReport(), nil
	}
	var fixCalls int
	doctorApplyFixes = func(context.Context, maintenance.Report, doctorRegistry, []*registry.WorktreeEntry) error {
		fixCalls++
		return nil
	}
	cmd, stdout, stderr := doctorTestCommand()

	err := runDoctor(cmd, nil)

	assertExitCode(t, err, 1)
	assert.Zero(t, fixCalls)
	assert.Contains(t, stdout.String(), "kwt doctor --fix")
	assert.Contains(t, stderr.String(), "kwt: inspect worktrees")
}

func TestDoctorHelpDescribesMaintenanceContract(t *testing.T) {
	var output bytes.Buffer
	doctorCmd.SetOut(&output)
	t.Cleanup(func() { doctorCmd.SetOut(nil) })

	require.NoError(t, doctorCmd.Help())

	help := output.String()
	assert.Contains(t, help, "read-only")
	assert.Contains(t, help, "--fix")
	assert.Contains(t, help, "--json")
	assert.Contains(t, help, "--quiet")
	assert.Contains(t, help, "missing generation")
	assert.Contains(t, help, "project registrations")
	assert.Contains(t, help, "confirmed-absent")
	assert.Contains(t, help, "relocate or remove")
	assert.Contains(t, help, "Git 2.31")
}

func TestDoctorInitializationFailureUsesMaintenanceErrorContract(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorJSON = true
	oldInitErr := configInitErr
	configInitErr = errors.New("config unavailable")
	t.Cleanup(func() { configInitErr = oldInitErr })
	cmd, stdout, stderr := doctorTestCommand()

	err := doctorCmd.PersistentPreRunE(cmd, nil)

	assertExitCode(t, err, 2)
	var envelope maintenanceErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, "doctor", envelope.Command)
	assert.Equal(t, "initialization_failed", envelope.Error.Code)
	assert.Contains(t, stderr.String(), "config unavailable")
}

func TestDoctorRejectsUnsupportedGitVersion(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonOutput], func(t *testing.T) {
			resetDoctorCommandDeps(t)
			doctorJSON = jsonOutput
			requireMaintenanceGitVersion = func() error {
				return errors.New("Git 2.31 or newer is required; found Git 2.30.2")
			}
			loadDoctorSnapshot = func() (*config.GlobalSnapshot, error) {
				t.Fatal("configuration must not load when Git is unsupported")
				return nil, nil
			}
			cmd, stdout, stderr := doctorTestCommand()

			err := runDoctor(cmd, nil)

			assertExitCode(t, err, 2)
			assert.Contains(t, stderr.String(), "unsupported_git_version")
			assert.Contains(t, stderr.String(), "Git 2.31")
			if jsonOutput {
				var envelope maintenanceErrorEnvelope
				require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
				assert.Equal(t, "doctor", envelope.Command)
				assert.Equal(t, "unsupported_git_version", envelope.Error.Code)
			} else {
				assert.Empty(t, stdout.String())
			}
		})
	}
}

func TestRenderDoctorReportUsesHumanDashboard(t *testing.T) {
	report := doctorFindingReport()
	report.Repositories[0].Findings[0].Evidence = map[string]string{
		"generation": "0123456789abcdef0123456789abcdef",
	}
	cmd, stdout, _ := doctorTestCommand()

	require.NoError(t, renderDoctorReport(cmd, report, false))

	output := stdout.String()
	assert.Contains(t, output, "kwt doctor")
	assert.Contains(t, output, "Ready to fix")
	assert.Contains(t, output, "Outdated worktree backlink")
	assert.NotContains(t, output, "0123456789abcdef0123456789abcdef")
}

func TestDoctorFixRescans(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorFix = true
	var inspections int
	doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
		inspections++
		if inspections == 1 {
			return doctorFindingReport(), nil
		}
		return doctorHealthyReport(), nil
	}
	var fixCalls int
	doctorApplyFixes = func(context.Context, maintenance.Report, doctorRegistry, []*registry.WorktreeEntry) error {
		fixCalls++
		return nil
	}
	cmd, stdout, _ := doctorTestCommand()

	err := runDoctor(cmd, nil)

	require.NoError(t, err)
	assert.Equal(t, 2, inspections)
	assert.Equal(t, 1, fixCalls)
	assert.Contains(t, stdout.String(), "Fixed  1")
	assert.Contains(t, stdout.String(), "/worktrees/topic")
	assert.Contains(t, stdout.String(), "No issues remain")
}

func TestDoctorFixJSONIncludesResolvedFindings(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorFix = true
	doctorJSON = true
	var inspections int
	doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
		inspections++
		if inspections == 1 {
			return doctorFindingReport(), nil
		}
		return doctorHealthyReport(), nil
	}
	doctorApplyFixes = func(context.Context, maintenance.Report, doctorRegistry, []*registry.WorktreeEntry) error {
		return nil
	}
	cmd, stdout, _ := doctorTestCommand()

	require.NoError(t, runDoctor(cmd, nil))
	var report maintenance.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Len(t, report.Fixed, 1)
	require.Len(t, report.Fixed[0].Findings, 1)
	assert.Equal(t, maintenance.BrokenWorktreeBacklink, report.Fixed[0].Findings[0].Code)
	assert.Equal(t, 1, report.Summary.FixedFindings)
	assert.Empty(t, report.Repositories)
}

func TestDoctorQuietSuppressesSuccessfulFixReport(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorFix = true
	doctorQuiet = true
	var inspections int
	doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
		inspections++
		if inspections == 1 {
			return doctorFindingReport(), nil
		}
		return doctorHealthyReport(), nil
	}
	doctorApplyFixes = func(context.Context, maintenance.Report, doctorRegistry, []*registry.WorktreeEntry) error {
		return nil
	}
	cmd, stdout, stderr := doctorTestCommand()

	require.NoError(t, runDoctor(cmd, nil))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
}

func TestDoctorRejectsQuietWithJSON(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorQuiet = true
	doctorJSON = true
	requireMaintenanceGitVersion = func() error {
		t.Fatal("version check must not hide incompatible flag usage")
		return nil
	}
	cmd, _, stderr := doctorTestCommand()

	err := runDoctor(cmd, nil)

	assertExitCode(t, err, 2)
	assert.Contains(t, stderr.String(), "mutually exclusive")
}

func TestDoctorFixReloadsConfigAndRegistryBeforeRescan(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorFix = true
	snapshots := []*config.GlobalSnapshot{
		{Config: &models.Config{Projects: []models.Project{{Path: "/old/widget"}}}},
		{Config: &models.Config{Projects: []models.Project{{Path: "/repos/widget"}}}},
	}
	var loads int
	loadDoctorSnapshot = func() (*config.GlobalSnapshot, error) {
		result := snapshots[loads]
		loads++
		return result, nil
	}
	var opens int
	openDoctorRegistry = func() (doctorRegistry, error) {
		opens++
		return &fakeDoctorRegistry{}, nil
	}
	var inspectedPaths []string
	doctorInspect = func(
		_ context.Context,
		cfg *models.Config,
		_ []config.ProjectRegistration,
		_ []*registry.WorktreeEntry,
		_ func(string) (bool, error),
	) (maintenance.Report, error) {
		inspectedPaths = append(inspectedPaths, cfg.Projects[0].Path)
		if len(inspectedPaths) == 1 {
			return doctorFindingReport(), nil
		}
		return doctorHealthyReport(), nil
	}
	doctorApplyFixes = func(
		context.Context,
		maintenance.Report,
		doctorRegistry,
		[]*registry.WorktreeEntry,
	) error {
		return nil
	}
	cmd, _, _ := doctorTestCommand()

	require.NoError(t, runDoctor(cmd, nil))
	assert.Equal(t, 2, loads)
	assert.Equal(t, 2, opens)
	assert.Equal(t, []string{"/old/widget", "/repos/widget"}, inspectedPaths)
}

func TestDoctorFixNoOpLeavesFinding(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorFix = true
	doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
		return doctorFindingReport(), nil
	}
	doctorApplyFixes = func(context.Context, maintenance.Report, doctorRegistry, []*registry.WorktreeEntry) error {
		return nil
	}
	cmd, _, _ := doctorTestCommand()

	err := runDoctor(cmd, nil)

	assertExitCode(t, err, 1)
}

func TestDoctorFixReloadFailuresExitTwo(t *testing.T) {
	tests := []struct {
		name        string
		configure   func()
		wantMessage string
	}{
		{
			name: "configuration reload",
			configure: func() {
				var loads int
				loadDoctorSnapshot = func() (*config.GlobalSnapshot, error) {
					loads++
					if loads == 2 {
						return nil, errors.New("config unavailable")
					}
					return &config.GlobalSnapshot{Config: &models.Config{}}, nil
				}
			},
			wantMessage: "reload global configuration after fixes",
		},
		{
			name: "registry reopen",
			configure: func() {
				var opens int
				openDoctorRegistry = func() (doctorRegistry, error) {
					opens++
					if opens == 2 {
						return nil, errors.New("registry unavailable")
					}
					return &fakeDoctorRegistry{}, nil
				}
			},
			wantMessage: "reopen worktree registry after fixes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDoctorCommandDeps(t)
			doctorFix = true
			doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
				return doctorFindingReport(), nil
			}
			doctorApplyFixes = func(context.Context, maintenance.Report, doctorRegistry, []*registry.WorktreeEntry) error {
				return nil
			}
			tt.configure()
			cmd, _, _ := doctorTestCommand()

			err := runDoctor(cmd, nil)

			assertExitCode(t, err, 2)
			assert.ErrorContains(t, err, tt.wantMessage)
		})
	}
}

func TestDoctorJSONEnvelope(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorJSON = true
	doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
		return doctorFindingReport(), nil
	}
	cmd, stdout, stderr := doctorTestCommand()

	err := runDoctor(cmd, nil)

	assertExitCode(t, err, 1)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, float64(1), envelope["schema_version"])
	assert.Equal(t, "doctor", envelope["command"])
	assert.Equal(t, false, envelope["fix"])
	assert.NotContains(t, stdout.String(), "Repository:")
	assert.Contains(t, stderr.String(), "kwt: inspect worktrees")
}

func TestDoctorExitCodes(t *testing.T) {
	tests := []struct {
		name       string
		report     maintenance.Report
		inspectErr error
		want       int
	}{
		{name: "healthy", report: doctorHealthyReport(), want: 0},
		{name: "findings", report: doctorFindingReport(), want: 1},
		{name: "inspection error", inspectErr: errors.New("inventory failed"), want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDoctorCommandDeps(t)
			doctorInspect = func(context.Context, *models.Config, []config.ProjectRegistration, []*registry.WorktreeEntry, func(string) (bool, error)) (maintenance.Report, error) {
				return tt.report, tt.inspectErr
			}
			cmd, _, _ := doctorTestCommand()

			err := runDoctor(cmd, nil)

			if tt.want == 0 {
				require.NoError(t, err)
				return
			}
			assertExitCode(t, err, tt.want)
		})
	}
}

func doctorFindingReport() maintenance.Report {
	return maintenance.Report{
		SchemaVersion: maintenance.SchemaVersion,
		Repositories: []maintenance.RepositoryReport{{
			Root: "/repos/widget", RepositoryIdentity: "github.com/acme/widget",
			Findings: []maintenance.Finding{{
				Code: maintenance.BrokenWorktreeBacklink, Path: "/worktrees/topic",
				Message: "broken backlink", Remediation: "Run kwt doctor --fix.", Fixable: true,
			}},
		}},
		Summary: maintenance.Summary{Findings: 1, FixableFindings: 1},
	}
}

func doctorHealthyReport() maintenance.Report {
	return maintenance.Report{
		SchemaVersion: maintenance.SchemaVersion,
		Summary:       maintenance.Summary{Healthy: true},
	}
}

func TestDoctorReportsInspectionAndVerificationProgress(t *testing.T) {
	resetDoctorCommandDeps(t)
	doctorFix = true
	oldTerminal := maintenanceProgressIsTerminal
	maintenanceProgressIsTerminal = func(io.Writer) bool { return false }
	t.Cleanup(func() { maintenanceProgressIsTerminal = oldTerminal })
	var inspections int
	doctorInspect = func(
		context.Context,
		*models.Config,
		[]config.ProjectRegistration,
		[]*registry.WorktreeEntry,
		func(string) (bool, error),
	) (maintenance.Report, error) {
		inspections++
		if inspections == 1 {
			return doctorFindingReport(), nil
		}
		return doctorHealthyReport(), nil
	}
	doctorApplyFixes = func(
		context.Context,
		maintenance.Report,
		doctorRegistry,
		[]*registry.WorktreeEntry,
	) error {
		return nil
	}
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
	oldTerminal := maintenanceProgressIsTerminal
	maintenanceProgressIsTerminal = func(io.Writer) bool { return false }
	t.Cleanup(func() { maintenanceProgressIsTerminal = oldTerminal })
	doctorInspect = func(
		context.Context,
		*models.Config,
		[]config.ProjectRegistration,
		[]*registry.WorktreeEntry,
		func(string) (bool, error),
	) (maintenance.Report, error) {
		return doctorHealthyReport(), nil
	}
	cmd, stdout, stderr := doctorTestCommand()

	require.NoError(t, runDoctor(cmd, nil))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
}

func doctorTestCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetContext(context.Background())
	return cmd, stdout, stderr
}

func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	require.Error(t, err)
	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, want, coded.ExitCode())
}

func resetDoctorCommandDeps(t *testing.T) {
	t.Helper()
	oldFix := doctorFix
	oldJSON := doctorJSON
	oldQuiet := doctorQuiet
	oldLoad := loadDoctorSnapshot
	oldOpen := openDoctorRegistry
	oldInspect := doctorInspect
	oldFixes := doctorApplyFixes
	oldRequireMaintenanceGitVersion := requireMaintenanceGitVersion
	oldSilenceUsage := rootCmd.SilenceUsage
	oldSilenceErrors := rootCmd.SilenceErrors
	t.Cleanup(func() {
		doctorFix = oldFix
		doctorJSON = oldJSON
		doctorQuiet = oldQuiet
		loadDoctorSnapshot = oldLoad
		openDoctorRegistry = oldOpen
		doctorInspect = oldInspect
		doctorApplyFixes = oldFixes
		requireMaintenanceGitVersion = oldRequireMaintenanceGitVersion
		rootCmd.SilenceUsage = oldSilenceUsage
		rootCmd.SilenceErrors = oldSilenceErrors
	})
	doctorFix = false
	doctorJSON = false
	doctorQuiet = false
	requireMaintenanceGitVersion = func() error { return nil }
	loadDoctorSnapshot = func() (*config.GlobalSnapshot, error) {
		return &config.GlobalSnapshot{Config: &models.Config{}}, nil
	}
	openDoctorRegistry = func() (doctorRegistry, error) { return &fakeDoctorRegistry{}, nil }
}

type fakeDoctorRegistry struct{}

func (*fakeDoctorRegistry) List() []*registry.WorktreeEntry                     { return nil }
func (*fakeDoctorRegistry) UnregisterIfGeneration(string, string) (bool, error) { return false, nil }
func (*fakeDoctorRegistry) CompareAndSwap(string, *registry.WorktreeEntry, *registry.WorktreeEntry) (bool, error) {
	return false, nil
}
func (*fakeDoctorRegistry) CompareAndSwapAliases(
	[]*registry.WorktreeEntry,
	*registry.WorktreeEntry,
) (bool, error) {
	return false, nil
}
func (*fakeDoctorRegistry) AcquireCreation(string) (func() error, bool, error) {
	return func() error { return nil }, true, nil
}
func (*fakeDoctorRegistry) CreationActive(string) (bool, error) { return false, nil }
