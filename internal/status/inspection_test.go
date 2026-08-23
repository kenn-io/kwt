package status

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitpkg "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type inspectionTestInventory struct {
	query func(context.Context, lifecycle.Request) (lifecycle.Result, error)
}

const (
	inspectionTestGeneration      = "0123456789abcdef0123456789abcdef"
	inspectionTestOtherGeneration = "fedcba9876543210fedcba9876543210"
)

func (f *inspectionTestInventory) Query(
	ctx context.Context,
	request lifecycle.Request,
) (lifecycle.Result, error) {
	return f.query(ctx, request)
}

func (*inspectionTestInventory) ApproveConfig(
	context.Context,
	lifecycle.ConfigApproval,
) error {
	return nil
}

func TestInspectionServiceRejectsInvalidRequestBeforeInventory(t *testing.T) {
	inventory := &inspectionTestInventory{query: func(
		context.Context,
		lifecycle.Request,
	) (lifecycle.Result, error) {
		t.Fatal("invalid request reached inventory")
		return lifecycle.Result{}, nil
	}}
	inspector := NewInspectionService(InspectionServiceOptions{Inventory: inventory})

	tests := []struct {
		name    string
		request InspectionRequest
	}{
		{name: "relative path", request: InspectionRequest{Path: "relative"}},
		{
			name: "malformed expected generation",
			request: InspectionRequest{
				Path: t.TempDir(), ExpectedGeneration: "not-a-generation",
			},
		},
		{
			name: "malformed expected repository",
			request: InspectionRequest{
				Path: t.TempDir(), ExpectedRepository: "not a repository",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := inspector.Inspect(context.Background(), tt.request)

			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.InvalidRequest))
			assert.False(t, service.AsError(err).Retryable)
		})
	}
}

func TestInspectionServiceRequiresOneExactInventoryEntry(t *testing.T) {
	path := t.TempDir()
	tests := []struct {
		name    string
		entries []lifecycle.Entry
		code    service.Code
	}{
		{name: "missing", code: service.NotFound},
		{
			name: "duplicate canonical path",
			entries: []lifecycle.Entry{
				inspectionInventoryEntry(path),
				inspectionInventoryEntry(path),
			},
			code: service.InspectionFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := &inspectionTestInventory{query: func(
				_ context.Context,
				request lifecycle.Request,
			) (lifecycle.Result, error) {
				assert.Equal(t, lifecycle.ViewRepository, request.View)
				assert.Equal(t, path, request.WorkingDirectory)
				assert.True(t, request.RequireCurrent)
				return lifecycle.Result{Snapshot: lifecycle.Snapshot{
					Entries: tt.entries,
				}}, nil
			}}

			_, err := NewInspectionService(InspectionServiceOptions{
				Inventory: inventory,
			}).Inspect(context.Background(), InspectionRequest{Path: path})

			require.Error(t, err)
			assert.True(t, service.IsCode(err, tt.code))
		})
	}
}

func TestInspectionServiceRejectsExpectedIdentityDrift(t *testing.T) {
	path := t.TempDir()
	inventory := &inspectionTestInventory{query: func(
		context.Context,
		lifecycle.Request,
	) (lifecycle.Result, error) {
		return lifecycle.Result{Snapshot: lifecycle.Snapshot{
			Entries: []lifecycle.Entry{inspectionInventoryEntry(path)},
		}}, nil
	}}
	inspector := NewInspectionService(InspectionServiceOptions{Inventory: inventory})

	tests := []struct {
		name    string
		request InspectionRequest
	}{
		{
			name: "repository",
			request: InspectionRequest{
				Path: path, ExpectedRepository: "github.com/acme/other",
			},
		},
		{
			name: "generation",
			request: InspectionRequest{
				Path: path, ExpectedGeneration: inspectionTestOtherGeneration,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := inspector.Inspect(context.Background(), tt.request)

			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.RegistrationChanged))
			assert.True(t, service.AsError(err).Retryable)
		})
	}
}

func TestInspectionServiceRejectsIncompleteInventoryIdentity(t *testing.T) {
	path := t.TempDir()
	tests := []struct {
		name  string
		entry lifecycle.Entry
	}{
		{
			name: "missing repository",
			entry: lifecycle.Entry{
				Path: path, Generation: inspectionTestGeneration,
			},
		},
		{
			name: "missing generation",
			entry: lifecycle.Entry{
				Path: path,
				Repository: lifecycle.Repository{
					FullPath: "github.com/acme/widget",
				},
			},
		},
		{
			name: "malformed generation",
			entry: lifecycle.Entry{
				Path:       path,
				Generation: "malformed",
				Repository: lifecycle.Repository{
					FullPath: "github.com/acme/widget",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := &inspectionTestInventory{query: func(
				context.Context,
				lifecycle.Request,
			) (lifecycle.Result, error) {
				return lifecycle.Result{Snapshot: lifecycle.Snapshot{
					Entries: []lifecycle.Entry{tt.entry},
				}}, nil
			}}

			_, err := NewInspectionService(InspectionServiceOptions{
				Inventory: inventory,
			}).Inspect(context.Background(), InspectionRequest{Path: path})

			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.InspectionFailed))
			assert.False(t, service.AsError(err).Retryable)
		})
	}
}

func TestInspectionServicePreservesTypedInventoryError(t *testing.T) {
	want := service.NewError(
		service.InventoryTimeout,
		"inventory refresh timed out",
		true,
		nil,
		context.DeadlineExceeded,
	)
	inventory := &inspectionTestInventory{query: func(
		context.Context,
		lifecycle.Request,
	) (lifecycle.Result, error) {
		return lifecycle.Result{}, want
	}}

	_, err := NewInspectionService(InspectionServiceOptions{
		Inventory: inventory,
	}).Inspect(context.Background(), InspectionRequest{Path: t.TempDir()})

	require.Error(t, err)
	assert.Same(t, want, err)
}

func TestInspectionServiceHidesUnexpectedInventoryError(t *testing.T) {
	cause := errors.New("credential-bearing private failure")
	inventory := &inspectionTestInventory{query: func(
		context.Context,
		lifecycle.Request,
	) (lifecycle.Result, error) {
		return lifecycle.Result{}, cause
	}}

	_, err := NewInspectionService(InspectionServiceOptions{
		Inventory: inventory,
	}).Inspect(context.Background(), InspectionRequest{Path: t.TempDir()})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.InspectionFailed))
	assert.NotContains(t, err.Error(), "credential-bearing")
	assert.ErrorIs(t, err, cause)
}

func TestInspectionServiceReturnsOnlyDoubleCheckedGeneration(t *testing.T) {
	path := t.TempDir()
	observedAt := time.Date(2026, 8, 20, 15, 4, 5, 0, time.UTC)
	inventory := inspectionInventoryWithConfig(path, "CUSTOM_FLEET_TOKEN")
	reads := 0
	inspector := inspectionServiceForTest(
		inventory,
		func(context.Context, string, []string) (string, error) {
			reads++
			return inspectionTestGeneration, nil
		},
		func(_ context.Context, gotPath string, protectedNames []string) (ChangeSet, error) {
			assert.Equal(t, path, gotPath)
			assert.ElementsMatch(t, []string{
				"KWT_GITHUB_TOKEN",
				"KWT_FLEET_TOKEN",
				"CUSTOM_FLEET_TOKEN",
			}, protectedNames)
			return ChangeSet{
				State: ChangeStateModified,
				Summary: ChangeSummary{
					Modified: 1,
				},
				Files: []FileChange{{
					Path: "changed.txt", Worktree: FileStateModified,
				}},
			}, nil
		},
		func() time.Time { return observedAt },
	)

	got, err := inspector.Inspect(context.Background(), InspectionRequest{
		Path:               path,
		ExpectedRepository: "github.com/acme/widget",
		ExpectedGeneration: inspectionTestGeneration,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, reads)
	assert.Equal(t, InspectionResult{
		Worktree: WorktreeIdentity{
			Repository: "github.com/acme/widget",
			Path:       path,
			Generation: inspectionTestGeneration,
		},
		Changes: ChangeSet{
			State: ChangeStateModified,
			Summary: ChangeSummary{
				Modified: 1,
			},
			Files: []FileChange{{
				Path: "changed.txt", Worktree: FileStateModified,
			}},
		},
		ObservedAt: observedAt,
	}, got)
}

func TestInspectionServiceProductionInventoryCarriesConfiguredProtectedNames(t *testing.T) {
	home := t.TempDir()
	repository := t.TempDir()
	runStatusTestGit(t, repository, "init", "-b", "main")
	runStatusTestGit(t, repository, "config", "user.name", "Test User")
	runStatusTestGit(t, repository, "config", "user.email", "test@example.com")
	runStatusTestGit(t, repository, "commit", "--allow-empty", "-m", "initial")
	generation, err := gitpkg.New(repository).EnsureWorktreeGeneration(repository)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[fleet]\ntoken_env = 'CUSTOM_FLEET_TOKEN'\n"),
		0o600,
	))
	inventory := lifecycle.NewInventoryService(lifecycle.InventoryServiceOptions{
		Source: lifecycle.NewSource(lifecycle.SourceOptions{Home: home}),
	})
	var observed [][]string
	inspector := NewInspectionService(InspectionServiceOptions{
		Inventory: inventory,
	}).(*inspectionService)
	inspector.readGeneration = func(
		_ context.Context,
		_ string,
		protectedNames []string,
	) (string, error) {
		observed = append(observed, append([]string(nil), protectedNames...))
		return generation, nil
	}
	inspector.collectChanges = func(
		_ context.Context,
		_ string,
		protectedNames []string,
	) (ChangeSet, error) {
		observed = append(observed, append([]string(nil), protectedNames...))
		return ChangeSet{State: ChangeStateClean, Files: []FileChange{}}, nil
	}

	_, err = inspector.Inspect(context.Background(), InspectionRequest{Path: repository})

	require.NoError(t, err)
	require.Len(t, observed, 3)
	for _, protectedNames := range observed {
		assert.Contains(t, protectedNames, "CUSTOM_FLEET_TOKEN")
	}
}

func TestInspectionServiceRejectsMissingInventoryConfigBeforeGit(t *testing.T) {
	path := t.TempDir()
	inventory := &inspectionTestInventory{query: func(
		context.Context,
		lifecycle.Request,
	) (lifecycle.Result, error) {
		return lifecycle.Result{Snapshot: lifecycle.Snapshot{
			Entries: []lifecycle.Entry{inspectionInventoryEntry(path)},
		}}, nil
	}}
	gitCalled := false
	inspector := inspectionServiceForTest(
		inventory,
		func(context.Context, string, []string) (string, error) {
			gitCalled = true
			return inspectionTestGeneration, nil
		},
		func(context.Context, string, []string) (ChangeSet, error) {
			gitCalled = true
			return ChangeSet{State: ChangeStateClean, Files: []FileChange{}}, nil
		},
		time.Now,
	)

	got, err := inspector.Inspect(
		context.Background(),
		InspectionRequest{Path: path},
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.InspectionFailed))
	assert.False(t, service.AsError(err).Retryable)
	assert.False(t, gitCalled)
	assert.Equal(t, InspectionResult{}, got)
}

func TestInspectionServiceRejectsGenerationDriftBeforeCollection(t *testing.T) {
	path := t.TempDir()
	collected := false
	inspector := inspectionServiceForTest(
		inspectionInventoryWithConfig(path, ""),
		func(context.Context, string, []string) (string, error) {
			return inspectionTestOtherGeneration, nil
		},
		func(context.Context, string, []string) (ChangeSet, error) {
			collected = true
			return ChangeSet{}, nil
		},
		time.Now,
	)

	_, err := inspector.Inspect(context.Background(), InspectionRequest{Path: path})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.False(t, collected)
}

func TestInspectionServiceDiscardsChangesWhenGenerationDriftsAfterCollection(t *testing.T) {
	path := t.TempDir()
	reads := 0
	inspector := inspectionServiceForTest(
		inspectionInventoryWithConfig(path, ""),
		func(context.Context, string, []string) (string, error) {
			reads++
			if reads == 1 {
				return inspectionTestGeneration, nil
			}
			return inspectionTestOtherGeneration, nil
		},
		func(context.Context, string, []string) (ChangeSet, error) {
			return ChangeSet{
				State: ChangeStateModified,
				Files: []FileChange{{
					Path: "must-not-escape.txt", Worktree: FileStateModified,
				}},
			}, nil
		},
		time.Now,
	)

	got, err := inspector.Inspect(context.Background(), InspectionRequest{Path: path})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.Equal(t, InspectionResult{}, got)
}

func TestInspectionServiceClassifiesReadAndCollectionFailures(t *testing.T) {
	path := t.TempDir()
	private := errors.New("private repository failure")
	tests := []struct {
		name       string
		generation func(context.Context, string, []string) (string, error)
		collect    func(context.Context, string, []string) (ChangeSet, error)
		wantCode   service.Code
	}{
		{
			name: "generation pre-read",
			generation: func(context.Context, string, []string) (string, error) {
				return "", private
			},
			collect: func(context.Context, string, []string) (ChangeSet, error) {
				t.Fatal("collection ran after failed generation pre-read")
				return ChangeSet{}, nil
			},
			wantCode: service.InspectionFailed,
		},
		{
			name: "generation missing before collection",
			generation: func(context.Context, string, []string) (string, error) {
				return "", errors.Join(private, gitpkg.ErrWorktreeGenerationNotFound)
			},
			collect: func(context.Context, string, []string) (ChangeSet, error) {
				t.Fatal("collection ran after missing generation pre-read")
				return ChangeSet{}, nil
			},
			wantCode: service.RegistrationChanged,
		},
		{
			name: "change collection",
			generation: func(context.Context, string, []string) (string, error) {
				return inspectionTestGeneration, nil
			},
			collect: func(context.Context, string, []string) (ChangeSet, error) {
				return ChangeSet{}, private
			},
			wantCode: service.InspectionFailed,
		},
		{
			name: "generation post-read",
			generation: func() func(context.Context, string, []string) (string, error) {
				reads := 0
				return func(context.Context, string, []string) (string, error) {
					reads++
					if reads == 1 {
						return inspectionTestGeneration, nil
					}
					return "", private
				}
			}(),
			collect: func(context.Context, string, []string) (ChangeSet, error) {
				return ChangeSet{State: ChangeStateClean, Files: []FileChange{}}, nil
			},
			wantCode: service.InspectionFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspector := inspectionServiceForTest(
				inspectionInventoryWithConfig(path, ""),
				tt.generation,
				tt.collect,
				time.Now,
			)

			got, err := inspector.Inspect(
				context.Background(),
				InspectionRequest{Path: path},
			)

			require.Error(t, err)
			assert.True(t, service.IsCode(err, tt.wantCode))
			assert.NotContains(t, err.Error(), "private repository failure")
			assert.ErrorIs(t, err, private)
			assert.Equal(t, InspectionResult{}, got)
		})
	}
}

func TestInspectionServiceReportsOversizedChangeList(t *testing.T) {
	path := t.TempDir()
	inspector := inspectionServiceForTest(
		inspectionInventoryWithConfig(path, ""),
		func(context.Context, string, []string) (string, error) {
			return inspectionTestGeneration, nil
		},
		func(context.Context, string, []string) (ChangeSet, error) {
			return ChangeSet{}, fmt.Errorf(
				"collect local changes: %w",
				gitpkg.ErrStdoutLimitExceeded,
			)
		},
		time.Now,
	)

	got, err := inspector.Inspect(
		context.Background(),
		InspectionRequest{Path: path},
	)

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.InspectionFailed))
	assert.Equal(t, "worktree change list is too large to inspect", err.Error())
	assert.ErrorIs(t, err, gitpkg.ErrStdoutLimitExceeded)
	assert.Equal(t, InspectionResult{}, got)
}

func TestInspectionServiceRechecksGenerationAfterCollectionFailure(t *testing.T) {
	path := t.TempDir()
	collectionFailure := errors.New("status collection failed")
	for _, tt := range []struct {
		name       string
		generation string
		err        error
	}{
		{
			name:       "generation changed",
			generation: inspectionTestOtherGeneration,
		},
		{
			name: "generation disappeared",
			err:  fmt.Errorf("generation disappeared: %w", gitpkg.ErrWorktreeGenerationNotFound),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reads := 0
			inspector := inspectionServiceForTest(
				inspectionInventoryWithConfig(path, ""),
				func(context.Context, string, []string) (string, error) {
					reads++
					if reads == 1 {
						return inspectionTestGeneration, nil
					}
					return tt.generation, tt.err
				},
				func(context.Context, string, []string) (ChangeSet, error) {
					return ChangeSet{}, collectionFailure
				},
				time.Now,
			)

			got, err := inspector.Inspect(
				context.Background(),
				InspectionRequest{Path: path},
			)

			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.RegistrationChanged))
			assert.True(t, service.AsError(err).Retryable)
			assert.NotErrorIs(t, err, collectionFailure)
			assert.Equal(t, 2, reads)
			assert.Equal(t, InspectionResult{}, got)
		})
	}
}

func TestInspectionServiceStopsBeforeInventoryWhenCanceled(t *testing.T) {
	private := errors.New("inventory must not run")
	inventory := &inspectionTestInventory{query: func(
		context.Context,
		lifecycle.Request,
	) (lifecycle.Result, error) {
		return lifecycle.Result{}, private
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewInspectionService(InspectionServiceOptions{
		Inventory: inventory,
	}).Inspect(ctx, InspectionRequest{Path: t.TempDir()})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, private)
}

func TestInspectionServicePreservesCancellationAfterInventoryReturns(t *testing.T) {
	path := t.TempDir()
	for _, tt := range []struct {
		name   string
		result lifecycle.Result
		err    error
	}{
		{
			name: "successful empty inventory",
			result: lifecycle.Result{Snapshot: lifecycle.Snapshot{
				Entries: []lifecycle.Entry{},
			}},
		},
		{
			name: "typed inventory error",
			err: service.NewError(
				service.NotFound,
				"inventory result must not replace cancellation",
				false,
				nil,
				nil,
			),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			inventory := &inspectionTestInventory{query: func(
				context.Context,
				lifecycle.Request,
			) (lifecycle.Result, error) {
				cancel()
				return tt.result, tt.err
			}}

			got, err := NewInspectionService(InspectionServiceOptions{
				Inventory: inventory,
			}).Inspect(ctx, InspectionRequest{Path: path})

			assert.Equal(t, context.Canceled, err)
			assert.Equal(t, InspectionResult{}, got)
		})
	}
}

func TestInspectionServicePreservesCancellationDuringGitInspection(t *testing.T) {
	for _, stage := range []string{"generation pre-read", "collection", "generation post-read"} {
		t.Run(stage, func(t *testing.T) {
			path := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			reads := 0
			readGeneration := func(
				context.Context,
				string,
				[]string,
			) (string, error) {
				reads++
				if stage == "generation pre-read" ||
					(stage == "generation post-read" && reads == 2) {
					cancel()
					return "", ctx.Err()
				}
				return inspectionTestGeneration, nil
			}
			collect := func(
				context.Context,
				string,
				[]string,
			) (ChangeSet, error) {
				if stage == "collection" {
					cancel()
					return ChangeSet{}, ctx.Err()
				}
				return ChangeSet{State: ChangeStateClean, Files: []FileChange{}}, nil
			}
			inspector := inspectionServiceForTest(
				inspectionInventoryWithConfig(path, ""),
				readGeneration,
				collect,
				time.Now,
			)

			got, err := inspector.Inspect(ctx, InspectionRequest{Path: path})

			require.Error(t, err)
			assert.Equal(t, context.Canceled, err)
			assert.Equal(t, InspectionResult{}, got)
		})
	}
}

func TestInspectionServicePreservesCallerDeadlineDuringGitInspection(t *testing.T) {
	path := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	inspector := inspectionServiceForTest(
		inspectionInventoryWithConfig(path, ""),
		func(
			callCtx context.Context,
			_ string,
			_ []string,
		) (string, error) {
			<-callCtx.Done()
			return "", callCtx.Err()
		},
		func(context.Context, string, []string) (ChangeSet, error) {
			t.Fatal("collection ran after caller deadline")
			return ChangeSet{}, nil
		},
		time.Now,
	)
	inspector.gitBudget = time.Second

	got, err := inspector.Inspect(ctx, InspectionRequest{Path: path})

	assert.Equal(t, context.DeadlineExceeded, err)
	assert.Equal(t, InspectionResult{}, got)
}

func TestInspectionServiceClassifiesOwnGitBudgetTimeoutAsRetryable(t *testing.T) {
	for _, stage := range []string{"generation pre-read", "collection", "generation post-read"} {
		t.Run(stage, func(t *testing.T) {
			path := t.TempDir()
			reads := 0
			readGeneration := func(
				ctx context.Context,
				_ string,
				_ []string,
			) (string, error) {
				reads++
				if stage == "generation pre-read" ||
					(stage == "generation post-read" && reads == 2) {
					<-ctx.Done()
					return "", ctx.Err()
				}
				return inspectionTestGeneration, nil
			}
			collect := func(
				ctx context.Context,
				_ string,
				_ []string,
			) (ChangeSet, error) {
				if stage == "collection" {
					<-ctx.Done()
					return ChangeSet{}, ctx.Err()
				}
				return ChangeSet{State: ChangeStateClean, Files: []FileChange{}}, nil
			}
			inspector := inspectionServiceForTest(
				inspectionInventoryWithConfig(path, ""),
				readGeneration,
				collect,
				time.Now,
			)
			inspector.gitBudget = 10 * time.Millisecond

			got, err := inspector.Inspect(
				context.Background(),
				InspectionRequest{Path: path},
			)

			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.InspectionFailed))
			assert.True(t, service.AsError(err).Retryable)
			assert.Equal(t, "worktree inspection timed out", err.Error())
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Equal(t, InspectionResult{}, got)
		})
	}
}

func TestInspectionServiceRejectsSuccessfulStageAfterOwnGitBudgetExpires(t *testing.T) {
	for _, stage := range []string{"generation pre-read", "collection", "generation post-read"} {
		t.Run(stage, func(t *testing.T) {
			path := t.TempDir()
			reads := 0
			readGeneration := func(
				ctx context.Context,
				_ string,
				_ []string,
			) (string, error) {
				reads++
				if stage == "generation pre-read" ||
					(stage == "generation post-read" && reads == 2) {
					<-ctx.Done()
				}
				return inspectionTestGeneration, nil
			}
			collect := func(
				ctx context.Context,
				_ string,
				_ []string,
			) (ChangeSet, error) {
				if stage == "collection" {
					<-ctx.Done()
				}
				return ChangeSet{State: ChangeStateClean, Files: []FileChange{}}, nil
			}
			inspector := inspectionServiceForTest(
				inspectionInventoryWithConfig(path, ""),
				readGeneration,
				collect,
				time.Now,
			)
			inspector.gitBudget = 10 * time.Millisecond

			got, err := inspector.Inspect(
				context.Background(),
				InspectionRequest{Path: path},
			)

			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.InspectionFailed))
			assert.True(t, service.AsError(err).Retryable)
			assert.Equal(t, "worktree inspection timed out", err.Error())
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Equal(t, InspectionResult{}, got)
		})
	}
}

func TestInspectionServiceRejectsMissingInventoryDependency(t *testing.T) {
	assert.NotPanics(t, func() {
		_, err := NewInspectionService(InspectionServiceOptions{}).Inspect(
			context.Background(),
			InspectionRequest{Path: t.TempDir()},
		)

		require.Error(t, err)
		assert.True(t, service.IsCode(err, service.InspectionFailed))
	})
}

func TestInspectionServiceInspectsRealPrimaryAndLinkedWorktrees(t *testing.T) {
	primary, linked, primaryGeneration, linkedGeneration :=
		newInspectionTestWorktrees(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(primary, "tracked.txt"),
		[]byte("modified\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(linked, "untracked.txt"),
		[]byte("untracked\n"),
		0o644,
	))
	var requestPath string
	if runtime.GOOS == "windows" {
		requestPath = strings.ToUpper(primary)
	} else {
		requestPath = filepath.Join(filepath.Dir(primary), "primary-alias")
		require.NoError(t, os.Symlink(primary, requestPath))
	}
	inventory := &inspectionTestInventory{query: func(
		context.Context,
		lifecycle.Request,
	) (lifecycle.Result, error) {
		return lifecycle.Result{Snapshot: lifecycle.Snapshot{
			Config: &models.Config{},
			Entries: []lifecycle.Entry{
				{
					Path: primary, Generation: primaryGeneration,
					Repository: lifecycle.Repository{FullPath: "local/test-repository"},
				},
				{
					Path: linked, Generation: linkedGeneration,
					Repository: lifecycle.Repository{FullPath: "local/test-repository"},
				},
			},
		}}, nil
	}}
	inspector := NewInspectionService(InspectionServiceOptions{Inventory: inventory})

	primaryResult, err := inspector.Inspect(context.Background(), InspectionRequest{
		Path: requestPath, ExpectedGeneration: primaryGeneration,
	})
	require.NoError(t, err)
	linkedResult, err := inspector.Inspect(context.Background(), InspectionRequest{
		Path: linked, ExpectedGeneration: linkedGeneration,
	})
	require.NoError(t, err)

	assert.Equal(t, primary, primaryResult.Worktree.Path)
	assert.Equal(t, primaryGeneration, primaryResult.Worktree.Generation)
	assert.Equal(t, ChangeSummary{Modified: 1}, primaryResult.Changes.Summary)
	assert.Equal(t, []string{"tracked.txt"}, changePaths(primaryResult.Changes.Files))
	assert.False(t, primaryResult.ObservedAt.IsZero())
	assert.Equal(t, linked, linkedResult.Worktree.Path)
	assert.Equal(t, linkedGeneration, linkedResult.Worktree.Generation)
	assert.Equal(t, ChangeSummary{Untracked: 1}, linkedResult.Changes.Summary)
	assert.Equal(t, []string{"untracked.txt"}, changePaths(linkedResult.Changes.Files))
}

func TestInspectionServiceRejectsRealGenerationReplacementOrRemoval(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "replacement",
			mutate: func(t *testing.T, generationPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(
					generationPath,
					[]byte(inspectionTestOtherGeneration+"\n"),
					0o600,
				))
			},
		},
		{
			name: "removal",
			mutate: func(t *testing.T, generationPath string) {
				t.Helper()
				require.NoError(t, os.Remove(generationPath))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary, _, generation, _ := newInspectionTestWorktrees(t)
			inventory := &inspectionTestInventory{query: func(
				context.Context,
				lifecycle.Request,
			) (lifecycle.Result, error) {
				return lifecycle.Result{Snapshot: lifecycle.Snapshot{
					Config: &models.Config{},
					Entries: []lifecycle.Entry{{
						Path: primary, Generation: generation,
						Repository: lifecycle.Repository{
							FullPath: "local/test-repository",
						},
					}},
				}}, nil
			}}
			serviceImpl := NewInspectionService(InspectionServiceOptions{
				Inventory: inventory,
			}).(*inspectionService)
			serviceImpl.collectChanges = func(
				context.Context,
				string,
				[]string,
			) (ChangeSet, error) {
				tt.mutate(t, filepath.Join(primary, ".git", "kwt-generation"))
				return ChangeSet{
					State: ChangeStateModified,
					Files: []FileChange{{
						Path: "must-not-escape.txt", Worktree: FileStateModified,
					}},
				}, nil
			}

			got, err := serviceImpl.Inspect(
				context.Background(),
				InspectionRequest{Path: primary, ExpectedGeneration: generation},
			)

			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.RegistrationChanged))
			assert.Equal(t, InspectionResult{}, got)
		})
	}
}

func TestInspectionServiceClassifiesRemovedWorktreeAroundGenerationFence(
	t *testing.T,
) {
	for _, stage := range []string{"before pre-read", "before post-read"} {
		t.Run(stage, func(t *testing.T) {
			_, linked, _, generation := newInspectionTestWorktrees(t)
			inventory := &inspectionTestInventory{query: func(
				context.Context,
				lifecycle.Request,
			) (lifecycle.Result, error) {
				result := lifecycle.Result{Snapshot: lifecycle.Snapshot{
					Config: &models.Config{},
					Entries: []lifecycle.Entry{{
						Path:       linked,
						Generation: generation,
						Repository: lifecycle.Repository{
							FullPath: "local/test-repository",
						},
					}},
				}}
				if stage == "before pre-read" {
					require.NoError(t, os.RemoveAll(linked))
				}
				return result, nil
			}}
			serviceImpl := NewInspectionService(InspectionServiceOptions{
				Inventory: inventory,
			}).(*inspectionService)
			serviceImpl.collectChanges = func(
				context.Context,
				string,
				[]string,
			) (ChangeSet, error) {
				if stage == "before pre-read" {
					t.Fatal("collection ran after the worktree was removed")
				}
				require.NoError(t, os.RemoveAll(linked))
				return ChangeSet{State: ChangeStateClean, Files: []FileChange{}}, nil
			}

			got, err := serviceImpl.Inspect(
				context.Background(),
				InspectionRequest{Path: linked, ExpectedGeneration: generation},
			)

			require.Error(t, err)
			assert.True(t, service.IsCode(err, service.RegistrationChanged))
			assert.True(t, service.AsError(err).Retryable)
			assert.ErrorIs(t, err, gitpkg.ErrWorktreeNotFound)
			assert.Equal(t, InspectionResult{}, got)
		})
	}
}

func inspectionInventoryWithConfig(
	path string,
	tokenEnv string,
) *inspectionTestInventory {
	return &inspectionTestInventory{query: func(
		context.Context,
		lifecycle.Request,
	) (lifecycle.Result, error) {
		return lifecycle.Result{Snapshot: lifecycle.Snapshot{
			Config: &models.Config{Fleet: models.FleetConfig{TokenEnv: tokenEnv}},
			Entries: []lifecycle.Entry{
				inspectionInventoryEntry(path),
			},
		}}, nil
	}}
}

func inspectionServiceForTest(
	inventory lifecycle.Inventory,
	readGeneration func(context.Context, string, []string) (string, error),
	collectChanges func(context.Context, string, []string) (ChangeSet, error),
	now func() time.Time,
) *inspectionService {
	return &inspectionService{
		inventory: inventory,
		captureExpansion: func() (lifecycle.ExpansionContext, error) {
			return lifecycle.ExpansionContext{
				WorkingDirectory: "/test/work",
				HomeDirectory:    "/test/home",
			}, nil
		},
		readGeneration: readGeneration,
		collectChanges: collectChanges,
		gitBudget:      collectChangesTimeout,
		now:            now,
	}
}

func inspectionInventoryEntry(path string) lifecycle.Entry {
	return lifecycle.Entry{
		Path:       path,
		Generation: inspectionTestGeneration,
		Repository: lifecycle.Repository{FullPath: "github.com/acme/widget"},
	}
}

func newInspectionTestWorktrees(t *testing.T) (string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	linked := filepath.Join(root, "linked")
	require.NoError(t, os.Mkdir(primary, 0o755))
	runStatusTestGit(t, primary, "init", "-b", "main")
	runStatusTestGit(t, primary, "config", "user.name", "Test User")
	runStatusTestGit(t, primary, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(
		filepath.Join(primary, "tracked.txt"),
		[]byte("original\n"),
		0o644,
	))
	runStatusTestGit(t, primary, "add", "tracked.txt")
	runStatusTestGit(
		t,
		primary,
		"-c",
		"core.hooksPath=/dev/null",
		"commit",
		"-m",
		"initial",
	)
	runStatusTestGit(t, primary, "worktree", "add", "-b", "feature", linked)
	primaryGeneration, err := gitpkg.New(primary).EnsureWorktreeGeneration(primary)
	require.NoError(t, err)
	linkedGeneration, err := gitpkg.New(primary).EnsureWorktreeGeneration(linked)
	require.NoError(t, err)
	return primary, linked, primaryGeneration, linkedGeneration
}
