package lifecycle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func TestPublishedProjectRegistrationsStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := publishedProjectRegistrations(ctx, []config.ProjectRegistration{{
		Persisted: models.Project{Path: t.TempDir()},
	}})

	require.ErrorIs(t, err, context.Canceled)
}

func TestSourceClassifiesInventoryFailuresWithActionableMessage(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[[projects]\n"),
		0o600,
	))

	_, err := NewSource(SourceOptions{Home: home}).Load(context.Background(), Request{
		View: ViewProjects, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
	})

	var typed *service.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, service.InventoryFailed, typed.Code)
	assert.False(t, typed.Retryable)
	assert.NotEqual(t, "internal failure", typed.Message)
	assert.Contains(t, typed.Message, "config")
	assert.LessOrEqual(t, len(typed.Message), 512)
}

func TestClassifyInventoryErrorPreservesTimeoutAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		code  service.Code
	}{
		{name: "deadline", cause: context.DeadlineExceeded, code: service.InventoryTimeout},
		{name: "cancellation", cause: context.Canceled, code: service.InventoryFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := classifyInventoryError(test.cause)
			var typed *service.Error
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, test.code, typed.Code)
			assert.True(t, typed.Retryable)
			assert.ErrorIs(t, err, test.cause)
		})
	}
}

func TestClassifyInventoryErrorHidesUnexpectedCredentialBearingCause(t *testing.T) {
	const secret = "inventory-password"
	cause := errors.New("fetch https://user:" + secret + "@example.invalid/repository")

	err := classifyInventoryError(cause)

	typed := service.AsError(err)
	assert.Equal(t, service.Internal, typed.Code)
	assert.Equal(t, "internal failure", typed.Message)
	assert.NotContains(t, typed.Message, secret)
	assert.ErrorIs(t, err, cause)
}

func TestBoundedDiagnosticRemovesControlCharacters(t *testing.T) {
	got := boundedDiagnostic(errors.New("unsafe\nmessage\twith\rcontrols"))

	assert.Equal(t, "unsafe message with controls", got)
}

func TestScanRepositoriesBoundsConcurrencyAndPreservesOrder(t *testing.T) {
	paths := make([]string, maxDashboardRepositoryScans+4)
	for index := range paths {
		paths[index] = string(rune('a' + index))
	}
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	type scanOutcome struct {
		results []repositoryScanResult
		err     error
	}
	done := make(chan scanOutcome, 1)
	go func() {
		results, err := scanRepositories(context.Background(), paths, func(
			_ context.Context,
			path string,
		) ([]Entry, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			<-release
			return []Entry{{Path: path}}, nil
		})
		done <- scanOutcome{results: results, err: err}
	}()
	require.Eventually(t, func() bool {
		return active.Load() == maxDashboardRepositoryScans
	}, time.Second, time.Millisecond)
	assert.LessOrEqual(t, maximum.Load(), int32(maxDashboardRepositoryScans))
	close(release)
	outcome := <-done
	require.NoError(t, outcome.err)
	results := outcome.results
	require.Len(t, results, len(paths))
	for index, result := range results {
		require.NoError(t, result.err)
		require.Len(t, result.entries, 1)
		assert.Equal(t, paths[index], result.entries[0].Path)
	}
}

func TestScanRepositoriesPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := scanRepositories(ctx, []string{"repository"}, func(
			ctx context.Context,
			_ string,
		) ([]Entry, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("repository scan did not stop after cancellation")
	}
}

func testExpansion(t *testing.T) ExpansionContext {
	t.Helper()
	expansion, err := CaptureExpansionContext()
	require.NoError(t, err)
	return expansion
}

func TestSourceProjectsRetainsInaccessibleRegistrations(t *testing.T) {
	home := t.TempDir()
	repository := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Mkdir(repository, 0o755))
	command := exec.Command("git", "init", repository)
	require.NoError(t, command.Run())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nrepository = 'github.com/acme/repo'\nname = 'repo'\npath = '"+repository+"'\n\n"+
			"[[projects]]\nrepository = 'github.com/acme/missing'\nname = 'missing'\npath = '"+filepath.Join(t.TempDir(), "missing")+"'\n",
	), 0o600))

	result, err := NewSource(SourceOptions{Home: home}).Load(context.Background(), Request{
		View: ViewProjects, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
	})

	require.NoError(t, err)
	require.Len(t, result.Snapshot.Projects, 2)
	assert.Equal(t, "github.com/acme/repo", result.Snapshot.Projects[0].Repository)
	assert.Equal(t, "github.com/acme/missing", result.Snapshot.Projects[1].Repository)
	assert.Regexp(t, `^v1:[0-9a-f]{64}$`, result.Snapshot.Projects[0].RegistrationFingerprint)
	assert.Regexp(t, `^v1:[0-9a-f]{64}$`, result.Snapshot.Projects[1].RegistrationFingerprint)
}

func TestSourceProjectFingerprintChangesWithUnknownPersistedField(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	writeConfig := func(value string) {
		require.NoError(t, os.WriteFile(configPath, []byte(
			"[[projects]]\nrepository = 'github.com/acme/repo'\nname = 'repo'\npath = '/missing/repo'\nfuture = '"+value+"'\n",
		), 0o600))
	}
	load := func(view View) InventoryProject {
		result, err := NewSource(SourceOptions{Home: home}).Load(context.Background(), Request{
			View: view, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
		})
		require.NoError(t, err)
		require.Len(t, result.Snapshot.Projects, 1)
		return result.Snapshot.Projects[0]
	}

	writeConfig("one")
	projectsView := load(ViewProjects)
	globalView := load(ViewGlobal)
	assert.Equal(t, projectsView, globalView)

	writeConfig("two")
	changed := load(ViewProjects)
	assert.Equal(t, projectsView.Path, changed.Path)
	assert.Equal(t, projectsView.Repository, changed.Repository)
	assert.NotEqual(t, projectsView.RegistrationFingerprint, changed.RegistrationFingerprint)
}

func TestSourceProjectsUsesRequestExpansionForMissingLocalRegistration(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nname = 'local'\npath = '$PROJECT_ROOT/repo ' \n",
	), 0o600))
	expansion := testExpansion(t)
	expansion.Environment["PROJECT_ROOT"] = filepath.Join(t.TempDir(), "one")

	result, err := NewSource(SourceOptions{Home: home}).Load(context.Background(), Request{
		View: ViewProjects, Expansion: expansion, UntrustedConfig: IgnoreUntrustedConfig,
	})

	require.NoError(t, err)
	require.Len(t, result.Snapshot.Projects, 1)
	assert.Equal(t, "$PROJECT_ROOT/repo ", result.Snapshot.Projects[0].Path)
	assert.Equal(t,
		"local/"+filepath.ToSlash(strings.TrimPrefix(filepath.Join(expansion.Environment["PROJECT_ROOT"], "repo "), string(filepath.Separator))),
		result.Snapshot.Projects[0].Repository,
	)
}

func TestSourceSnapshotIncludesEffectiveGlobalConfig(t *testing.T) {
	home := t.TempDir()
	baseDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte("[worktree]\nbasedir = '"+baseDirectory+"'\n[layouts]\ndefault = 'quad'\n"),
		0o600,
	))

	result, err := NewSource(SourceOptions{Home: home}).Load(context.Background(), Request{
		View: ViewProjects, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
	})

	require.NoError(t, err)
	require.NotNil(t, result.Snapshot.Config)
	assert.Equal(t, baseDirectory, result.Snapshot.Config.Worktree.BaseDir)
	assert.Equal(t, "quad", result.Snapshot.Config.Layouts.Default)
}

func TestSourceRejectsRelativeRepositoryDirectory(t *testing.T) {
	_, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewRepository, WorkingDirectory: "relative", Expansion: testExpansion(t),
		UntrustedConfig: IgnoreUntrustedConfig,
	})
	assert.ErrorContains(t, err, "must be absolute")
}

func TestSourceRejectsMissingPathExpansionContext(t *testing.T) {
	_, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewProjects, UntrustedConfig: IgnoreUntrustedConfig,
	})

	assert.ErrorContains(t, err, "path-expansion working directory must be absolute")
}

func TestSourcePropagatesRepositoryInventoryErrors(t *testing.T) {
	workingDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workingDirectory, ".git"),
		[]byte("gitdir: /missing/repository\n"),
		0o600,
	))

	_, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewRepository, WorkingDirectory: workingDirectory, UntrustedConfig: IgnoreUntrustedConfig,
		Expansion: testExpansion(t),
	})

	require.Error(t, err)
}

func TestSourceRecognizesRepositoryThroughSymlinkedSubdirectory(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	for _, args := range [][]string{
		{"init", "-b", "main", repository},
		{"-C", repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"},
	} {
		command := exec.Command("git", args...)
		require.NoError(t, command.Run())
	}
	subdirectory := filepath.Join(repository, "nested", "directory")
	require.NoError(t, os.MkdirAll(subdirectory, 0o755))
	linkedDirectory := filepath.Join(t.TempDir(), "linked-directory")
	if err := os.Symlink(subdirectory, linkedDirectory); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewRepository, WorkingDirectory: linkedDirectory,
		UntrustedConfig: IgnoreUntrustedConfig, Expansion: testExpansion(t),
	})

	require.NoError(t, err)
	require.NotEmpty(t, result.Snapshot.Entries)
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	require.NoError(t, err)
	assert.Equal(t, canonicalRepository, result.Snapshot.Entries[0].Path)
}

func TestSourceRepositoryInventoryIgnoresInheritedGitRouting(t *testing.T) {
	createRepository := func(path string) {
		t.Helper()
		for _, args := range [][]string{
			{"init", "-b", "main", path},
			{"-C", path, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"},
		} {
			command := exec.Command("git", args...)
			require.NoError(t, command.Run())
		}
	}
	target := filepath.Join(t.TempDir(), "target")
	redirected := filepath.Join(t.TempDir(), "redirected")
	createRepository(target)
	createRepository(redirected)
	t.Setenv("GIT_DIR", filepath.Join(redirected, ".git"))

	result, err := NewSource(SourceOptions{Home: t.TempDir()}).Load(context.Background(), Request{
		View: ViewRepository, WorkingDirectory: target,
		UntrustedConfig: IgnoreUntrustedConfig, Expansion: testExpansion(t),
	})

	require.NoError(t, err)
	require.NotEmpty(t, result.Snapshot.Entries)
	canonicalTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	assert.Equal(t, canonicalTarget, result.Snapshot.Entries[0].Path)
}

func TestSourceReadsProvenanceOnlyWhenRequested(t *testing.T) {
	home := t.TempDir()
	baseDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[worktree]\nbasedir = '"+baseDirectory+"'\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "pull-requests.json"), []byte("{"), 0o600))
	source := NewSource(SourceOptions{Home: home})

	_, err := source.Load(context.Background(), Request{
		View: ViewDashboard, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
	})
	require.NoError(t, err)

	_, err = source.Load(context.Background(), Request{
		View: ViewDashboard, Expansion: testExpansion(t), UntrustedConfig: IgnoreUntrustedConfig,
		IncludeProtectedSockets: true,
	})
	require.ErrorContains(t, err, "failed to read pull-request provenance")
}

func TestSourceSeparatesLaunchInventoryFromDashboardEntries(t *testing.T) {
	launchDirectory := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main", launchDirectory},
		{"-C", launchDirectory, "remote", "add", "origin", "https://github.com/acme/launch.git"},
		{"-C", launchDirectory, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"},
	} {
		command := exec.Command("git", args...)
		require.NoError(t, command.Run())
	}
	source := &currentSource{}

	entries, launchEntries, err := source.loadDashboard(context.Background(), Request{
		LaunchDirectory: launchDirectory,
	}, &models.Config{Worktree: models.WorktreeConfig{BaseDir: t.TempDir()}})

	require.NoError(t, err)
	require.NotEmpty(t, entries)
	require.NotEmpty(t, launchEntries)
	canonicalLaunchDirectory, err := filepath.EvalSymlinks(launchDirectory)
	require.NoError(t, err)
	for _, entry := range launchEntries {
		assert.Equal(t, canonicalLaunchDirectory, entry.Path)
		assert.Equal(t, "github.com/acme/launch", entry.Repository.FullPath)
	}
}

func TestDashboardLaunchInventoryPropagatesRefreshDeadline(t *testing.T) {
	launchDirectory := filepath.Join(t.TempDir(), "launch")
	for _, args := range [][]string{
		{"init", "-b", "main", launchDirectory},
		{"-C", launchDirectory, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"},
	} {
		command := exec.Command("git", args...)
		require.NoError(t, command.Run())
	}
	lock := flock.New(
		filepath.Join(launchDirectory, ".git", "kwt-worktree.lock"),
		flock.SetPermissions(0o600),
	)
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { _ = lock.Unlock() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := (&currentSource{}).loadDashboard(ctx, Request{
		LaunchDirectory: launchDirectory,
	}, &models.Config{Worktree: models.WorktreeConfig{BaseDir: t.TempDir()}})

	require.ErrorIs(t, err, context.DeadlineExceeded)
}
