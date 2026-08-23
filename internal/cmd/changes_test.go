package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

const changesTestGeneration = "0123456789abcdef0123456789abcdef"

type changesInspectorFunc func(
	context.Context,
	kwt.InspectionRequest,
) (kwt.InspectionResult, error)

func (f changesInspectorFunc) Inspect(
	ctx context.Context,
	request kwt.InspectionRequest,
) (kwt.InspectionResult, error) {
	return f(ctx, request)
}

func TestRunChangesDefaultsToAbsoluteCwdAndWritesInspectionJSON(t *testing.T) {
	resetChangesCommand(t)
	worktree := t.TempDir()
	t.Chdir(worktree)
	wantObserved := time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC)
	var gotRequest kwt.InspectionRequest
	newChangesInspector = func(*cobra.Command) kwt.Inspector {
		return changesInspectorFunc(func(
			_ context.Context,
			request kwt.InspectionRequest,
		) (kwt.InspectionResult, error) {
			gotRequest = request
			return kwt.InspectionResult{
				Worktree: kwt.WorktreeIdentity{
					Repository: "github.com/acme/widget",
					Path:       worktree,
					Generation: changesTestGeneration,
				},
				Changes: kwt.ChangeSet{
					State: kwt.ChangeStateClean,
					Files: []kwt.FileChange{},
				},
				ObservedAt: wantObserved,
			}, nil
		})
	}
	changesJSON = true
	var stdout, stderr bytes.Buffer
	changesCmd.SetOut(&stdout)
	changesCmd.SetErr(&stderr)

	err := runChanges(changesCmd, nil)

	require.NoError(t, err)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	wantPath, err := filepath.Abs(cwd)
	require.NoError(t, err)
	assert.Equal(t, kwt.InspectionRequest{Path: wantPath}, gotRequest)
	var got kwt.InspectionResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, "github.com/acme/widget", got.Worktree.Repository)
	assert.Equal(t, changesTestGeneration, got.Worktree.Generation)
	assert.Equal(t, wantObserved, got.ObservedAt)
	assert.NotNil(t, got.Changes.Files)
	assert.Empty(t, got.Changes.Files)
	assert.Empty(t, stderr.String())
}

func TestRunChangesPassesAbsoluteLiteralPathAndIndependentGuards(t *testing.T) {
	resetChangesCommand(t)
	root := t.TempDir()
	t.Chdir(root)
	literal := filepath.Join("nested", "..", "linked")
	wantPath, err := filepath.Abs(literal)
	require.NoError(t, err)
	var gotRequest kwt.InspectionRequest
	newChangesInspector = func(*cobra.Command) kwt.Inspector {
		return changesInspectorFunc(func(
			_ context.Context,
			request kwt.InspectionRequest,
		) (kwt.InspectionResult, error) {
			gotRequest = request
			return cleanChangesResult(wantPath), nil
		})
	}
	changesJSON = true
	changesExpectedRepository = "github.com/acme/widget"
	changesExpectedGeneration = changesTestGeneration
	changesCmd.SetOut(&bytes.Buffer{})
	changesCmd.SetErr(&bytes.Buffer{})

	err = runChanges(changesCmd, []string{literal})

	require.NoError(t, err)
	assert.Equal(t, kwt.InspectionRequest{
		Path:               wantPath,
		ExpectedRepository: "github.com/acme/widget",
		ExpectedGeneration: changesTestGeneration,
	}, gotRequest)
}

func TestRunChangesWritesDeterministicHumanOutput(t *testing.T) {
	resetChangesCommand(t)
	observed := time.Date(2026, 8, 20, 18, 0, 0, 123, time.UTC)
	result := kwt.InspectionResult{
		Worktree: kwt.WorktreeIdentity{
			Repository: "local/acme\twidget",
			Path:       "/worktrees/widget\nlinked",
			Generation: changesTestGeneration,
		},
		Changes: kwt.ChangeSet{
			State: kwt.ChangeStateConflicted,
			Summary: kwt.ChangeSummary{
				Modified: 1, Staged: 1, Conflicts: 1,
			},
			Files: []kwt.FileChange{
				{
					Path:     "conflict\tname.txt",
					Index:    kwt.FileStateConflicted,
					Worktree: kwt.FileStateConflicted,
				},
				{
					Path:         "renamed\nname.txt",
					OriginalPath: "old name.txt",
					Index:        kwt.FileStateRenamed,
				},
			},
		},
		ObservedAt: observed,
	}
	newChangesInspector = func(*cobra.Command) kwt.Inspector {
		return changesInspectorFunc(func(
			context.Context,
			kwt.InspectionRequest,
		) (kwt.InspectionResult, error) {
			return result, nil
		})
	}
	var stdout, stderr bytes.Buffer
	changesCmd.SetOut(&stdout)
	changesCmd.SetErr(&stderr)

	err := runChanges(changesCmd, []string{"/worktrees/widget"})

	require.NoError(t, err)
	assert.Equal(t, `Repository: "local/acme\twidget"
Worktree: "/worktrees/widget\nlinked"
Generation: 0123456789abcdef0123456789abcdef
Observed at: 2026-08-20T18:00:00.000000123Z
Changes:
  "conflict\tname.txt"
    staged: -
    working tree: conflicted
  "old name.txt" -> "renamed\nname.txt"
    staged: renamed
    working tree: -
`, stdout.String())
	assert.Empty(t, stderr.String())
}

func TestRunChangesCleanHumanOutputSaysNoChangedFiles(t *testing.T) {
	resetChangesCommand(t)
	result := cleanChangesResult("/worktrees/widget")
	newChangesInspector = func(*cobra.Command) kwt.Inspector {
		return changesInspectorFunc(func(
			context.Context,
			kwt.InspectionRequest,
		) (kwt.InspectionResult, error) {
			return result, nil
		})
	}
	var stdout bytes.Buffer
	changesCmd.SetOut(&stdout)
	changesCmd.SetErr(&bytes.Buffer{})

	err := runChanges(changesCmd, []string{"/worktrees/widget"})

	require.NoError(t, err)
	assert.Equal(t, `Repository: "github.com/acme/widget"
Worktree: "/worktrees/widget"
Generation: 0123456789abcdef0123456789abcdef
Observed at: 2026-08-20T18:00:00Z
No changed files
`, stdout.String())
}

func TestChangesInitializationFailureUsesSharedJSONEnvelope(t *testing.T) {
	resetChangesCommand(t)
	changesJSON = true
	originalInitErr := configInitErr
	privateConfigErr := errors.New("private config path: /private/config.toml")
	configInitErr = privateConfigErr
	t.Cleanup(func() { configInitErr = originalInitErr })
	var stdout, stderr bytes.Buffer
	changesCmd.SetOut(&stdout)
	changesCmd.SetErr(&stderr)

	err := changesCmd.PersistentPreRunE(changesCmd, nil)

	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 1, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.InspectionFailed, envelope.Error.Code)
	assert.False(t, envelope.Error.Retryable)
	assert.Equal(t, "failed to initialize configuration", envelope.Error.Message)
	assert.Equal(
		t,
		"kwt changes: inspection_failed: failed to initialize configuration\n",
		stderr.String(),
	)
	assert.ErrorIs(t, err, privateConfigErr)
}

func TestRunChangesWritesStableJSONFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		code      service.Code
		retryable bool
		exitCode  int
	}{
		{name: "invalid request", code: service.InvalidRequest, exitCode: 2},
		{name: "missing worktree", code: service.NotFound, exitCode: 2},
		{
			name: "repository mismatch", code: service.RegistrationChanged,
			retryable: true, exitCode: 1,
		},
		{name: "malformed inventory", code: service.InspectionFailed, exitCode: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetChangesCommand(t)
			newChangesInspector = func(*cobra.Command) kwt.Inspector {
				return changesInspectorFunc(func(
					context.Context,
					kwt.InspectionRequest,
				) (kwt.InspectionResult, error) {
					return kwt.InspectionResult{}, service.NewError(
						tt.code,
						"stable failure",
						tt.retryable,
						nil,
						errors.New("private cause"),
					)
				})
			}
			changesJSON = true
			var stdout, stderr bytes.Buffer
			changesCmd.SetOut(&stdout)
			changesCmd.SetErr(&stderr)

			err := runChanges(changesCmd, []string{"/worktrees/widget"})

			var coded interface{ ExitCode() int }
			require.ErrorAs(t, err, &coded)
			assert.Equal(t, tt.exitCode, coded.ExitCode())
			var envelope jsonErrorEnvelope
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
			assert.Equal(t, tt.code, envelope.Error.Code)
			assert.Equal(t, tt.retryable, envelope.Error.Retryable)
			assert.NotContains(t, stdout.String(), "private cause")
			assert.Equal(t, "kwt changes: "+string(tt.code)+": stable failure\n", stderr.String())
		})
	}
}

func TestRunChangesPreservesCallerContextErrors(t *testing.T) {
	for _, contextErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(contextErr.Error(), func(t *testing.T) {
			resetChangesCommand(t)
			newChangesInspector = func(*cobra.Command) kwt.Inspector {
				return changesInspectorFunc(func(
					context.Context,
					kwt.InspectionRequest,
				) (kwt.InspectionResult, error) {
					return kwt.InspectionResult{}, contextErr
				})
			}
			changesJSON = true
			var stdout, stderr bytes.Buffer
			changesCmd.SetOut(&stdout)
			changesCmd.SetErr(&stderr)

			err := runChanges(changesCmd, []string{"/worktrees/widget"})

			require.ErrorIs(t, err, contextErr)
			assert.Empty(t, stdout.String())
			assert.Empty(t, stderr.String())
		})
	}
}

func TestRunChangesWritesTypedInspectionTimeoutAsJSONFailure(t *testing.T) {
	resetChangesCommand(t)
	newChangesInspector = func(*cobra.Command) kwt.Inspector {
		return changesInspectorFunc(func(
			context.Context,
			kwt.InspectionRequest,
		) (kwt.InspectionResult, error) {
			return kwt.InspectionResult{}, service.NewError(
				service.InspectionFailed,
				"worktree inspection timed out",
				true,
				nil,
				context.DeadlineExceeded,
			)
		})
	}
	changesJSON = true
	var stdout, stderr bytes.Buffer
	changesCmd.SetOut(&stdout)
	changesCmd.SetErr(&stderr)

	err := runChanges(changesCmd, []string{"/worktrees/widget"})

	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 1, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.InspectionFailed, envelope.Error.Code)
	assert.True(t, envelope.Error.Retryable)
	assert.Equal(t, "worktree inspection timed out", envelope.Error.Message)
	assert.Equal(
		t,
		"kwt changes: inspection_failed: worktree inspection timed out\n",
		stderr.String(),
	)
}

func TestChangesArgumentFailureUsesSharedJSONEnvelope(t *testing.T) {
	resetChangesCommand(t)
	changesJSON = true
	var stdout, stderr bytes.Buffer
	changesCmd.SetOut(&stdout)
	changesCmd.SetErr(&stderr)

	err := changesCmd.Args(changesCmd, []string{"one", "two"})

	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 2, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.InvalidRequest, envelope.Error.Code)
	assert.Equal(t, "kwt changes: invalid_request: expected at most one worktree path\n", stderr.String())
}

func TestChangesFlagFailureFindsJSONAfterUnknownFlag(t *testing.T) {
	resetChangesCommand(t)
	originalArgs := os.Args
	os.Args = []string{"kwt", "changes", "--bogus", "--json"}
	t.Cleanup(func() { os.Args = originalArgs })
	var stdout, stderr bytes.Buffer
	changesCmd.SetOut(&stdout)
	changesCmd.SetErr(&stderr)
	flagError := changesCmd.FlagErrorFunc()
	require.NotNil(t, flagError)

	err := flagError(changesCmd, errors.New("unknown flag: --bogus"))

	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 2, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.InvalidRequest, envelope.Error.Code)
	assert.Equal(t, "kwt changes: invalid_request: unknown flag: --bogus\n", stderr.String())
}

func TestChangesHelpDocumentsFocusedInspectionAndGuards(t *testing.T) {
	help := changesCmd.Long + "\n" + changesCmd.Example

	assert.Contains(t, help, "one exact worktree")
	assert.Contains(t, help, "does not fetch")
	assert.Contains(t, help, "kwt changes [path] --json")
	assert.Contains(t, help, "--expected-repository")
	assert.Contains(t, help, "--expected-generation")
}

func cleanChangesResult(path string) kwt.InspectionResult {
	return kwt.InspectionResult{
		Worktree: kwt.WorktreeIdentity{
			Repository: "github.com/acme/widget",
			Path:       path,
			Generation: changesTestGeneration,
		},
		Changes: kwt.ChangeSet{
			State: kwt.ChangeStateClean,
			Files: []kwt.FileChange{},
		},
		ObservedAt: time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC),
	}
}

func resetChangesCommand(t *testing.T) {
	t.Helper()
	originalFactory := newChangesInspector
	originalJSON := changesJSON
	originalRepository := changesExpectedRepository
	originalGeneration := changesExpectedGeneration
	originalOut := changesCmd.OutOrStdout()
	originalErr := changesCmd.ErrOrStderr()
	originalSilenceUsage := rootCmd.SilenceUsage
	originalSilenceErrors := rootCmd.SilenceErrors
	t.Cleanup(func() {
		newChangesInspector = originalFactory
		changesJSON = originalJSON
		changesExpectedRepository = originalRepository
		changesExpectedGeneration = originalGeneration
		changesCmd.SetOut(originalOut)
		changesCmd.SetErr(originalErr)
		rootCmd.SilenceUsage = originalSilenceUsage
		rootCmd.SilenceErrors = originalSilenceErrors
	})
	changesJSON = false
	changesExpectedRepository = ""
	changesExpectedGeneration = ""
}
