package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/maintenance"
)

func TestRenderDoctorHumanOrganizesFindingsByAction(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	report := doctorOutputReport(generation)

	output := renderDoctorHuman(report, doctorRenderOptions{
		Width: 80, Color: false, HomeDir: "/home/user-a", TempDir: "/tmp",
	})

	summary := strings.Index(output, "3 issues")
	ready := strings.Index(output, "Ready to fix")
	manual := strings.Index(output, "Needs review")
	require.GreaterOrEqual(t, summary, 0, output)
	assert.Greater(t, ready, summary, output)
	assert.Greater(t, manual, ready, output)
	assert.Contains(t, output, "Project moved")
	assert.Contains(t, output, "Stale registry entry")
	assert.Contains(t, output, "Project location is ambiguous")
	assert.Contains(t, output, "github.com/acme/widget")
	assert.Contains(t, output, "Registry")
	assert.NotContains(t, output, "github.com/acme/healthy")
	assert.Contains(t, output, filepath.Join("~", "old", "widget")+" → /repos/widget")
	assert.Contains(t, output, filepath.Join("$TMPDIR", "kwt-stale"))
	assert.Contains(t, output, "/repos/widget-one")
	assert.Contains(t, output, "/repos/widget-two")
	assert.NotContains(t, output, generation)
	assert.NotContains(t, output, "\x1b[")
	assert.Equal(t, 1, strings.Count(output, "Run kwt doctor --fix to repair confirmed issues."), output)
}

func TestRenderDoctorHumanWrapsAndElidesAtNarrowWidth(t *testing.T) {
	report := doctorOutputReport("opaque-generation")
	report.Repositories[0].Findings[0].Evidence["old_path"] =
		"/home/user-a/code/github.com/acme/a-very-long-repository-name/widget"

	output := renderDoctorHuman(report, doctorRenderOptions{
		Width: 40, Color: false, HomeDir: "/home/user-a", TempDir: "/tmp",
	})

	assert.Contains(t, output, "~")
	assert.Contains(t, output, "…")
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		assert.LessOrEqual(t, runewidth.StringWidth(line), 40, "line exceeds width: %q", line)
	}
}

func TestRenderDoctorHumanShowsUnknownFinding(t *testing.T) {
	report := maintenance.Report{
		Repositories: []maintenance.RepositoryReport{{Findings: []maintenance.Finding{{
			Code: "future_finding", Path: "/worktrees/topic", Message: "future detail",
			Remediation: "Review the future finding.",
		}}}},
		Summary: maintenance.Summary{Findings: 1, ManualFindings: 1},
	}

	output := renderDoctorHuman(report, doctorRenderOptions{Width: 80})

	assert.Contains(t, output, "Future finding")
	assert.Contains(t, output, "future detail")
	assert.Contains(t, output, "Review the future finding.")
}

func TestDoctorPresentationLabelsUnverifiedRegistryEntry(t *testing.T) {
	presentation := doctorPresentation(maintenance.UnverifiedRegistryEntry)

	assert.True(t, presentation.Known)
	assert.Equal(t, "Unverified registry path", presentation.Title)
	assert.Contains(t, presentation.Action, "registry")
}

func TestRenderDoctorHumanShowsDuplicateRegistryAliases(t *testing.T) {
	aliases := []string{"/worktrees/topic", "/aliases/topic"}
	report := maintenance.Report{
		Repositories: []maintenance.RepositoryReport{{
			RepositoryIdentity: "github.com/acme/widget",
			Findings: []maintenance.Finding{{
				Code: maintenance.DuplicateRegistryEntry, Path: aliases[0],
				Message:     "multiple equivalent registry paths resolve to the same live worktree",
				Remediation: "Run kwt doctor --fix to collapse the unchanged aliases.",
				Fixable:     true,
				Evidence:    map[string]string{"paths": strings.Join(aliases, "\n")},
			}},
		}},
		Summary: maintenance.Summary{Findings: 1, FixableFindings: 1},
	}

	output := renderDoctorHuman(report, doctorRenderOptions{Width: 80})

	assert.Contains(t, output, "Duplicate registry paths")
	assert.Contains(t, output, "alias: /worktrees/topic")
	assert.Contains(t, output, "alias: /aliases/topic")
	assert.Contains(t, output, "Collapse equivalent aliases to one registry record.")
}

func TestRenderDoctorHumanUsesColorOnlyWhenEnabled(t *testing.T) {
	report := doctorOutputReport("opaque-generation")

	colored := renderDoctorHuman(report, doctorRenderOptions{Width: 80, Color: true})
	plain := renderDoctorHuman(report, doctorRenderOptions{Width: 80, Color: false})

	assert.Contains(t, colored, "\x1b[")
	assert.NotContains(t, plain, "\x1b[")
}

func TestRenderDoctorHumanKeepsManualFindingDetailsActionable(t *testing.T) {
	report := maintenance.Report{
		Repositories: []maintenance.RepositoryReport{
			{
				RepositoryIdentity: "github.com/acme/a-repository-identity-that-is-too-long-for-the-terminal",
				Findings: []maintenance.Finding{
					{
						Code: maintenance.RegistryGenerationMismatch, Path: "/worktrees/topic",
						Message:     "registry and Git generations identify different worktrees",
						Remediation: "Review the replacement conflict; do not transfer expiration policy.",
					},
					{
						Code: maintenance.RepositoryIdentityMismatch, Path: "/worktrees/identity",
						Message:     "registry identity does not match the live repository",
						Remediation: "Review the repository identities.",
						Evidence: map[string]string{
							"configured_repository": "github.com/acme/widget",
							"registry_repository":   "github.com/other/widget",
							"live_repository":       "github.com/user/widget",
							"git_generation":        "hidden-generation",
						},
					},
				},
			},
			{
				Root: "/a/very/long/repository/root/that/is/not/reachable",
				Findings: []maintenance.Finding{{
					Code: maintenance.ProjectUnreachable, Path: "/missing/widget",
					Message:     "configured project could not be inspected: permission denied",
					Remediation: "Restore filesystem access.",
				}},
			},
		},
		Summary: maintenance.Summary{Findings: 3, ManualFindings: 3},
	}

	output := renderDoctorHuman(report, doctorRenderOptions{Width: 52})

	assert.Contains(t, output, "Review the replacement conflict")
	assert.NotContains(t, output, "verified legacy")
	assert.Contains(t, output, "permission denied")
	assert.Contains(t, output, "configured: github.com/acme/widget")
	assert.Contains(t, output, "registry: github.com/other/widget")
	assert.Contains(t, output, "live: github.com/user/widget")
	assert.NotContains(t, output, "hidden-generation")
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		assert.LessOrEqual(t, runewidth.StringWidth(line), 52, "line exceeds width: %q", line)
	}
}

func TestRenderDoctorHumanShowsRepairsBeforeRemainingFindings(t *testing.T) {
	report := maintenance.Report{
		Fixed: []maintenance.RepositoryReport{{
			RepositoryIdentity: "github.com/acme/widget",
			Findings: []maintenance.Finding{{
				Code:    maintenance.BrokenWorktreeBacklink,
				Path:    "/worktrees/topic",
				Fixable: true,
				Evidence: map[string]string{
					"current_target":  "/old/.git/worktrees/topic",
					"expected_target": "/new/.git/worktrees/topic",
				},
			}},
		}},
		Summary: maintenance.Summary{FixedFindings: 1, Findings: 1, ManualFindings: 1},
		Repositories: []maintenance.RepositoryReport{{
			Root: "/worktrees/orphan",
			Findings: []maintenance.Finding{{
				Code: maintenance.ProjectUnreachable, Path: "/worktrees/orphan",
				Message: "repository could not be inspected", Remediation: "Inspect it manually.",
			}},
		}},
	}

	output := renderDoctorHuman(report, doctorRenderOptions{Width: 100})

	fixed := strings.Index(output, "Fixed  1")
	remaining := strings.Index(output, "Needs review  1")
	require.GreaterOrEqual(t, fixed, 0, output)
	assert.Greater(t, remaining, fixed, output)
	assert.Contains(t, output, "1 fixed · 1 issue remains")
	assert.Contains(t, output, "/old/.git/worktrees/topic → /new/.git/worktrees/topic")
	assert.NotContains(t, output[:remaining], "Next:")
}

func doctorOutputReport(generation string) maintenance.Report {
	return maintenance.Report{
		Repositories: []maintenance.RepositoryReport{
			{
				RepositoryIdentity: "github.com/acme/widget",
				Findings: []maintenance.Finding{
					{
						Code: maintenance.ProjectPathMoved, Path: "/home/user-a/old/widget",
						Message: "one matching repository was found", Fixable: true,
						Remediation: "Run kwt doctor --fix to update the project.",
						Evidence: map[string]string{
							"old_path": "/home/user-a/old/widget", "new_path": "/repos/widget",
						},
					},
					{
						Code: maintenance.AmbiguousProjectRelocation, Path: "/home/user-a/old/ambiguous",
						Message:     "multiple matching repositories were found",
						Remediation: "Choose the intended repository path.",
						Evidence: map[string]string{
							"candidate_paths": "/repos/widget-two\n/repos/widget-one",
						},
					},
				},
			},
			{
				Findings: []maintenance.Finding{{
					Code: maintenance.StaleRegistryEntry, Path: "/tmp/kwt-stale",
					Message: "registry path is absent", Fixable: true,
					Remediation: "Run kwt doctor --fix to remove the entry.",
					Evidence:    map[string]string{"generation": generation},
				}},
			},
			{RepositoryIdentity: "github.com/acme/healthy"},
		},
		Summary: maintenance.Summary{
			Findings: 3, FixableFindings: 2, ManualFindings: 1,
		},
	}
}
