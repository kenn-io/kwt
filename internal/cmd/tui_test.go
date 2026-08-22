package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/fleet"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	dashboard "go.kenn.io/kwt/internal/tui"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func resolveStoppedWorkspaceSessions(
	_ context.Context,
	requests []tmux.WorkspaceEndpointRequest,
) ([]tmux.WorkspaceSession, error) {
	sessions := make([]tmux.WorkspaceSession, len(requests))
	for index, request := range requests {
		sessions[index] = tmux.WorkspaceSession{
			Endpoint: testCanonicalSessionEndpoint(request.SessionName),
		}
	}
	return sessions, nil
}

func requireTUIBackendStateLocked(t *testing.T, backend *tuiBackend) {
	t.Helper()
	if backend.mu.TryLock() {
		backend.mu.Unlock()
		t.Fatal("TUI backend operation read mutable state without holding its mutex")
	}
}

func TestTUICmdIsolatesFromCwdConfig(t *testing.T) {
	require.NotNil(t, tuiCmd.PersistentPreRunE,
		"tui must define its own PersistentPreRunE to bypass root's cwd merge")
	require.NoError(t, tuiCmd.PersistentPreRunE(tuiCmd, nil),
		"tui's PersistentPreRunE must be a no-op that never errors")
}

func TestRunTUIRejectsNonInteractiveTerminal(t *testing.T) {
	oldStdin, oldStdout := stdinIsTerminal, stdoutIsTerminal
	defer func() {
		stdinIsTerminal = oldStdin
		stdoutIsTerminal = oldStdout
	}()
	stdinIsTerminal = func() bool { return false }
	stdoutIsTerminal = func() bool { return true }

	err := runTUI(tuiCmd, nil)

	require.Error(t, err)
	assert.EqualError(t, err, "kwt tui requires an interactive terminal")
}

func TestRootPersistentPreRunSkipsCwdConfigForBareRoot(t *testing.T) {
	oldMerge := mergeCwdLocal
	defer func() { mergeCwdLocal = oldMerge }()
	called := false
	mergeCwdLocal = func() error {
		called = true
		return nil
	}

	require.NoError(t, rootCmd.PersistentPreRunE(rootCmd, nil))

	assert.False(t, called, "bare root command must not merge cwd local config before launching global TUI")
}

func TestRootPersistentPreRunMergesCwdConfigForSubcommands(t *testing.T) {
	oldMerge := mergeCwdLocal
	defer func() { mergeCwdLocal = oldMerge }()
	called := false
	mergeCwdLocal = func() error {
		called = true
		return nil
	}

	require.NoError(t, rootCmd.PersistentPreRunE(statusCmd, nil))

	assert.True(t, called, "ordinary subcommands must still merge cwd local config")
}

func TestRootPersistentPreRunReturnsMergeError(t *testing.T) {
	oldMerge := mergeCwdLocal
	defer func() { mergeCwdLocal = oldMerge }()
	mergeCwdLocal = func() error { return errors.New("merge failed") }

	err := rootCmd.PersistentPreRunE(statusCmd, nil)

	require.Error(t, err)
	assert.EqualError(t, err, "merge failed")
}

func TestRootArgsRejectsUnknownBareArgs(t *testing.T) {
	require.NotNil(t, rootCmd.Args)
	assert.NoError(t, rootCmd.Args(rootCmd, nil))
	assert.Error(t, rootCmd.Args(rootCmd, []string{"unknown"}))
}

func TestRootRunPrintsHelpWhenNotInteractive(t *testing.T) {
	oldStdin, oldStdout, oldRun := stdinIsTerminal, stdoutIsTerminal, runRootTUI
	defer func() {
		stdinIsTerminal = oldStdin
		stdoutIsTerminal = oldStdout
		runRootTUI = oldRun
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	}()
	stdinIsTerminal = func() bool { return false }
	stdoutIsTerminal = func() bool { return false }
	launched := false
	runRootTUI = func(cmd *cobra.Command, args []string) error {
		launched = true
		return nil
	}
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	require.NoError(t, runRoot(rootCmd, nil))

	assert.False(t, launched)
	assert.Contains(t, out.String(), "kwt is a CLI tool")
}

func TestRootRunLaunchesTUIWhenInteractive(t *testing.T) {
	oldStdin, oldStdout, oldRun := stdinIsTerminal, stdoutIsTerminal, runRootTUI
	defer func() {
		stdinIsTerminal = oldStdin
		stdoutIsTerminal = oldStdout
		runRootTUI = oldRun
	}()
	stdinIsTerminal = func() bool { return true }
	stdoutIsTerminal = func() bool { return true }
	launched := false
	runRootTUI = func(cmd *cobra.Command, args []string) error {
		launched = true
		return nil
	}

	require.NoError(t, runRoot(rootCmd, nil))

	assert.True(t, launched)
}

func TestBuildTUIRowSkipsSessionNameWhenRepositoryInfoMissing(t *testing.T) {
	entry := &discovery.GlobalWorktreeEntry{
		Branch: "detached",
		Path:   "/work/odd-detached",
	}
	status := &models.WorktreeStatus{Path: entry.Path, Branch: entry.Branch}

	row := buildTUIRow(entry, status, tmux.WorkspaceSession{})

	assert.Equal(t, entry, row.Entry)
	assert.Equal(t, status, row.Status)
	assert.Empty(t, row.SessionName)
	assert.False(t, row.SessionLive)
}

func TestBuildTUIRowMarksLiveSessionWhenRepositoryInfoPresent(t *testing.T) {
	entry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{Host: "github.com", Owner: "example", Repository: "kwt"},
		Branch:         "feature",
		Path:           "/work/kwt/feature",
	}
	status := &models.WorktreeStatus{Path: entry.Path, Branch: entry.Branch}

	endpoint := testCanonicalSessionEndpoint(
		tmux.WorkspaceSessionName(entry.RepositoryInfo, entry.Branch, entry.Path),
	)
	row := buildTUIRow(entry, status, tmux.WorkspaceSession{Endpoint: endpoint})

	assert.NotEmpty(t, row.SessionName)
	assert.False(t, row.SessionLive)

	row = buildTUIRow(entry, status, tmux.WorkspaceSession{
		Endpoint: endpoint,
		Live:     true,
	})
	assert.True(t, row.SessionLive)
}

func TestTUIStatusCollectorOptionsFetchesSyncState(t *testing.T) {
	opts := tuiStatusCollectorOptions("/worktrees")

	assert.True(t, opts.FetchRemote)
	assert.Equal(t, "/worktrees", opts.BaseDir)
}

func TestCollectTUIStatusesSurfacesPerRowDiagnostic(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	entry := &discovery.GlobalWorktreeEntry{
		Path: missing, Branch: "topic",
		RepositoryInfo: &url.RepositoryInfo{FullPath: "github.com/acme/widget"},
	}

	statuses, warnings, err := collectTUIStatuses(context.Background(), t.TempDir(), []*discovery.GlobalWorktreeEntry{entry})

	require.NoError(t, err)
	require.Contains(t, statuses, missing)
	assert.Equal(t, models.WorktreeStatusUnknown, statuses[missing].Status)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "status unavailable for "+missing)
}

func TestReadTUIFleetStateReadsHubWithoutPublishingGlobalStatus(t *testing.T) {
	resetFleetCommandDeps(t)

	cfg := &models.Config{Fleet: models.FleetConfig{
		Enabled: true,
		HubURL:  "https://hub.example.test",
	}}
	sequence := []string{}
	client := &stubFleetClient{}
	publishFleetBestEffort = func(ctx context.Context, gotCfg *models.Config, builder fleet.ManifestBuildProvider, warn *bytes.Buffer) error {
		sequence = append(sequence, "publish")
		assert.Equal(t, cfg, gotCfg)
		assert.NotNil(t, builder)
		return errors.New("hub unavailable")
	}
	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		sequence = append(sequence, "builder")
		return &stubFleetManifestBuilder{}
	}
	newFleetClientFromConfig = func(gotCfg *models.Config) (fleetHubClient, error) {
		sequence = append(sequence, "client")
		assert.Same(t, cfg, gotCfg)
		client.sequence = &sequence
		return client, nil
	}

	_, err := readTUIFleetState(context.Background(), cfg)

	require.NoError(t, err)
	assert.Equal(t, []string{"client", "state"}, sequence)
}

func TestReadTUIFleetStateDoesNotBuildManifest(t *testing.T) {
	resetFleetCommandDeps(t)

	cfg := &models.Config{Fleet: models.FleetConfig{
		Enabled: true,
		HubURL:  "https://hub.example.test",
	}}
	client := &stubFleetClient{}
	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		t.Fatal("TUI fleet reads must not build a global manifest")
		return nil
	}
	publishFleetBestEffort = func(ctx context.Context, gotCfg *models.Config, builder fleet.ManifestBuildProvider, warn *bytes.Buffer) error {
		return fleet.PublishBestEffort(ctx, gotCfg, builder, warn)
	}
	newFleetClientFromConfig = func(gotCfg *models.Config) (fleetHubClient, error) {
		assert.Same(t, cfg, gotCfg)
		return client, nil
	}

	_, err := readTUIFleetState(context.Background(), cfg)

	require.NoError(t, err)
}

func TestTUIBackendListAndMergeFleetAreConcurrencySafe(t *testing.T) {
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Fleet:    models.FleetConfig{Enabled: true, HostID: "host-a"},
	}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{Host: "github.com", Owner: "example", Repository: "kwt"},
		Branch:         "main",
		Path:           "/repos/kwt",
		IsMain:         true,
	}
	backend := newTUIBackendWithLaunchDir(cfg, "/repos/kwt")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{launchEntry}, nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	// Read cfg.Projects the way the manifest builder does during publish.
	backend.readFleetState = func(ctx context.Context, cfg *models.Config) (fleet.FleetState, error) {
		for _, project := range cfg.Projects {
			_ = project.Repository
		}
		return fleet.FleetState{}, nil
	}

	// List mutates cfg.Projects (launch registration) while MergeFleet reads
	// it; run them concurrently so the race detector can catch unsynchronized
	// access.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, _, err := backend.List(context.Background())
			require.NoError(t, err)
			backend.MergeFleet(context.Background(), rows)
		}()
	}
	wg.Wait()
}

func TestTUIBackendListIncludesLaunchRepositoryWorktrees(t *testing.T) {
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	globalEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{Host: "github.com", Owner: "example", Repository: "kwt"},
		Branch:         "main",
		Path:           "/global/github.com/example/kwt/main",
	}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{Host: "github.com", Owner: "example", Repository: "other"},
		Branch:         "main",
		Path:           "/repos/other",
		IsMain:         true,
	}
	backend := newTUIBackendWithLaunchDir(cfg, "/repos/other")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		assert.Equal(t, "/global", baseDir)
		return []*discovery.GlobalWorktreeEntry{globalEntry}, nil
	}
	backend.discoverLaunchWorktrees = func(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		assert.Equal(t, "/repos/other", launchDir)
		return []*discovery.GlobalWorktreeEntry{launchEntry}, nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		assert.Equal(t, "/global", baseDir)
		assert.Len(t, entries, 2)
		return map[string]*models.WorktreeStatus{
			globalEntry.Path: {Path: globalEntry.Path, Branch: globalEntry.Branch},
			launchEntry.Path: {Path: launchEntry.Path, Branch: launchEntry.Branch, IsCurrent: true},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{globalEntry.Path, launchEntry.Path}, []string{
		rowPathForHandoff(rows[0]),
		rowPathForHandoff(rows[1]),
	})
	assert.True(t, rows[1].Status.IsCurrent)
}

func TestTUIBackendListFastSkipsStatusCollection(t *testing.T) {
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	entry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host: "github.com", Owner: "example", Repository: "kwt",
		},
		Branch: "main",
		Path:   "/global/github.com/example/kwt/main",
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{entry}, nil
	}
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.collectStatuses = func(
		context.Context,
		string,
		[]*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		t.Fatal("fast listing must not collect Git status")
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.ListFast(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, models.WorktreeStatusUnknown, rows[0].Status.Status)
}

func TestTUIBackendListCollectsStatusForImportedWorktree(t *testing.T) {
	entry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host: "github.com", Owner: "example", Repository: "kwt",
		},
		Branch: "feature/remote",
		Path:   "/global/github.com/example/kwt/feature-remote",
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
	}, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{entry}, nil
	}
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.collectStatuses = func(
		_ context.Context,
		_ string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		require.Equal(t, []*discovery.GlobalWorktreeEntry{entry}, entries)
		return map[string]*models.WorktreeStatus{
			entry.Path: {
				Path:   entry.Path,
				Branch: entry.Branch,
				Status: models.WorktreeStatusClean,
			},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, models.WorktreeStatusClean, rows[0].Status.Status)
}

func TestTUIBackendListFastRunsIndependentDiscoveryConcurrently(t *testing.T) {
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Projects: []models.Project{{Path: "/registered"}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "/launch")
	stubTUIProjectRegistration(backend)
	started := make(chan string, 3)
	release := make(chan struct{})
	block := func(name string) {
		started <- name
		<-release
	}
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		block("global")
		return nil, nil
	}
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		block("registered")
		return nil, nil
	}
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		block("launch")
		return nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	done := make(chan error, 1)
	go func() {
		_, _, err := backend.ListFast(context.Background())
		done <- err
	}()

	for range 3 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("independent startup discovery ran serially")
		}
	}
	close(release)
	require.NoError(t, <-done)
}

func TestTUIBackendListIncludesRegisteredProjectWorktrees(t *testing.T) {
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Projects: []models.Project{{
			Repository: "github.com/example/tools",
			Name:       "other",
			Path:       "/repos/other",
		}},
	}
	globalEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{Host: "github.com", Owner: "example", Repository: "kwt"},
		Branch:         "main",
		Path:           "/global/github.com/example/kwt/main",
	}
	projectEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{Host: "github.com", Owner: "example", Repository: "other"},
		Branch:         "feature",
		Path:           "/repos/other-feature",
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		assert.Equal(t, "/global", baseDir)
		return []*discovery.GlobalWorktreeEntry{globalEntry}, nil
	}
	backend.discoverProjectWorktrees = func(projectPath string) ([]*discovery.GlobalWorktreeEntry, error) {
		assert.Equal(t, "/repos/other", projectPath)
		return []*discovery.GlobalWorktreeEntry{projectEntry}, nil
	}
	backend.discoverLaunchWorktrees = func(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		require.Empty(t, launchDir)
		return nil, nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		assert.ElementsMatch(t, []string{globalEntry.Path, projectEntry.Path}, []string{
			entries[0].Path,
			entries[1].Path,
		})
		return map[string]*models.WorktreeStatus{
			globalEntry.Path:  {Path: globalEntry.Path, Branch: globalEntry.Branch},
			projectEntry.Path: {Path: projectEntry.Path, Branch: projectEntry.Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{globalEntry.Path, projectEntry.Path}, []string{
		rowPathForHandoff(rows[0]),
		rowPathForHandoff(rows[1]),
	})
}

func TestTUIBackendListPropagatesIncompleteRegisteredProjectInventory(
	t *testing.T,
) {
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Projects: []models.Project{{
			Repository: "github.com/example/tools",
			Path:       "/repos/tools",
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(
		string,
	) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	incomplete := &git.IncompleteInventoryError{
		Path: "/repos/tools",
		Err:  errors.New("generation is unreadable"),
	}
	backend.discoverProjectWorktrees = func(
		string,
	) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, incomplete
	}
	backend.discoverLaunchWorktrees = func(
		string,
	) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.ListFast(context.Background())

	require.Error(t, err)
	assert.ErrorIs(t, err, incomplete)
	assert.Nil(t, rows)
}

func TestDashboardFleetInfoSummarizesPrimaryObservations(t *testing.T) {
	tests := []struct {
		name         string
		observations []fleet.Observation
		local        bool
		want         bool
	}{
		{name: "empty", observations: nil, want: false},
		{
			name: "all primary",
			observations: []fleet.Observation{
				{HostID: "host-b", IsMain: true},
				{HostID: "host-c", IsMain: true},
			},
			want: true,
		},
		{
			name: "mixed",
			observations: []fleet.Observation{
				{HostID: "host-b", IsMain: true},
				{HostID: "host-c", IsMain: false},
			},
			want: false,
		},
		{
			name: "all linked",
			observations: []fleet.Observation{
				{HostID: "host-b"},
				{HostID: "host-c"},
			},
			want: false,
		},
		{
			name: "ignores synthesized local observation",
			observations: []fleet.Observation{
				{HostID: "host-a", IsMain: false},
				{HostID: "host-b", IsMain: true},
				{HostID: "host-c", IsMain: true},
			},
			local: true,
			want:  true,
		},
		{
			name: "local observation alone is not remote primary state",
			observations: []fleet.Observation{
				{HostID: "host-a", IsMain: true},
			},
			local: true,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := dashboardFleetInfo(fleet.FleetRow{
				ProjectIdentity: "github.com/example/kwt",
				Kind:            "branch",
				Ref:             "main",
				Branch:          "main",
				Observations:    tt.observations,
			}, fleet.StatusRow{}, "host-a", tt.local)

			require.NotNil(t, info)
			assert.Equal(t, tt.want, info.AllPrimary)
		})
	}
}

func TestDashboardFleetInfoKeepsDetachedMaterializeIdentityRaw(t *testing.T) {
	ref := strings.Repeat("a", 40)
	info := dashboardFleetInfo(fleet.FleetRow{
		ProjectIdentity: "github.com/example/kwt",
		Kind:            "detached",
		Ref:             ref,
		Observations: []fleet.Observation{{
			HostID: "host-b",
			IsMain: true,
		}},
	}, fleet.StatusRow{}, "host-a", false)

	require.NotNil(t, info)
	assert.True(t, info.AllPrimary)
	assert.Equal(t, ref, info.MaterializeLabel)
	assert.False(t, info.CanMaterialize)
}

func TestTUIBackendMergeFleetIncludesRemoteOnlyFleetRows(t *testing.T) {
	observedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Fleet:    models.FleetConfig{Enabled: true, HostID: "host-a"},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       "/repos/kwt",
		}},
	}
	localEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "kwt",
			FullPath:   "github.com/example/kwt",
		},
		Branch: "main",
		Path:   "/repos/kwt",
		IsMain: true,
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{localEntry}, nil
	}
	backend.discoverProjectWorktrees = func(projectPath string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverLaunchWorktrees = func(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{
			localEntry.Path: {Path: localEntry.Path, Branch: localEntry.Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.readFleetState = func(context.Context, *models.Config) (fleet.FleetState, error) {
		return fleet.FleetState{Rows: []fleet.FleetRow{
			{
				ProjectIdentity: "github.com/example/kwt",
				ProjectName:     "kwt",
				Kind:            "branch",
				Ref:             "main",
				Branch:          "main",
				Observations: []fleet.Observation{{
					HostID:     "host-a",
					Path:       "/repos/kwt",
					Head:       "aaa",
					ObservedAt: observedAt,
				}},
			},
			{
				ProjectIdentity: "github.com/example/kwt",
				ProjectName:     "kwt",
				Kind:            "branch",
				Ref:             "feature/studio-only",
				Branch:          "feature/studio-only",
				Observations: []fleet.Observation{{
					HostID:     "host-b",
					Path:       "/work/host-b/kwt/feature-studio-only",
					Head:       "bbb",
					Upstream:   "origin/feature/studio-only",
					Ahead:      2,
					ObservedAt: observedAt,
				}},
			},
		}}, nil
	}

	rows, _, err := backend.List(context.Background())
	require.NoError(t, err)
	rows, _ = backend.MergeFleet(context.Background(), rows)

	require.Len(t, rows, 2)
	remote := rows[0]
	if remote.Fleet == nil || remote.Fleet.Ref != "feature/studio-only" {
		remote = rows[1]
	}
	require.NotNil(t, remote.Fleet)
	assert.Nil(t, remote.Entry)
	assert.Equal(t, "kwt", remote.Fleet.ProjectName)
	assert.Equal(t, "feature/studio-only", remote.Fleet.Branch)
	assert.False(t, remote.Fleet.Local)
	assert.Equal(t, []string{"host-b"}, remote.Fleet.Hosts)
	assert.True(t, remote.Fleet.CanMaterialize,
		"a registered project matches this identity, so sync must be offered")
	assert.Equal(t, "host-b", remote.Fleet.MaterializeHost)
	assert.Equal(t, "/work/host-b/kwt/feature-studio-only", remote.Fleet.RemotePath)
	assert.Equal(t, "bbb", remote.Fleet.RemoteHead)
	assert.Equal(t, "origin/feature/studio-only", remote.Fleet.RemoteUpstream)
	assert.Equal(t, 2, remote.Fleet.RemoteAhead)
}

func TestTUIBackendMergeFleetDoesNotOfferSyncWithoutRegisteredProject(t *testing.T) {
	observedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Fleet:    models.FleetConfig{Enabled: true, HostID: "host-a"},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.readFleetState = func(context.Context, *models.Config) (fleet.FleetState, error) {
		return fleet.FleetState{Rows: []fleet.FleetRow{{
			ProjectIdentity: "github.com/example/kwt",
			ProjectName:     "kwt",
			Kind:            "branch",
			Ref:             "feature/studio-only",
			Branch:          "feature/studio-only",
			Observations: []fleet.Observation{{
				HostID:     "host-b",
				Path:       "/work/host-b/kwt/feature-studio-only",
				Head:       "bbb",
				ObservedAt: observedAt,
			}},
		}}}, nil
	}

	rows, _, err := backend.List(context.Background())
	require.NoError(t, err)
	rows, _ = backend.MergeFleet(context.Background(), rows)

	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Fleet)
	assert.False(t, rows[0].Fleet.CanMaterialize,
		"sync must not be offered when no registered project can host the worktree")
}

func TestTUIBackendMergeFleetLocalPresenceComesFromLocalDiscovery(t *testing.T) {
	observedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Fleet:    models.FleetConfig{Enabled: true, HostID: "host-a"},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       "/repos/kwt",
		}},
	}
	localEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "kwt",
			FullPath:   "github.com/example/kwt",
		},
		Branch: "main",
		Path:   "/repos/kwt",
		IsMain: true,
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{localEntry}, nil
	}
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{
			localEntry.Path: {Path: localEntry.Path, Branch: localEntry.Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.readFleetState = func(context.Context, *models.Config) (fleet.FleetState, error) {
		return fleet.FleetState{Rows: []fleet.FleetRow{
			{
				// Present locally, but the hub missed this host's publish.
				ProjectIdentity: "github.com/example/kwt",
				Kind:            "branch",
				Ref:             "main",
				Branch:          "main",
				Observations: []fleet.Observation{{
					HostID:     "host-b",
					Path:       "/work/host-b/kwt",
					Head:       "bbb",
					ObservedAt: observedAt,
				}},
			},
			{
				// Deleted locally, but a stale hub observation for this host
				// remains alongside a real one from another host.
				ProjectIdentity: "github.com/example/kwt",
				Kind:            "branch",
				Ref:             "feature/remote",
				Branch:          "feature/remote",
				Observations: []fleet.Observation{
					{HostID: "host-a", Path: "/repos/kwt-feature-remote", Head: "ccc", ObservedAt: observedAt},
					{HostID: "host-b", Path: "/work/host-b/kwt-feature-remote", Head: "ddd", ObservedAt: observedAt},
				},
			},
			{
				// Deleted locally and observed nowhere else: stale noise.
				ProjectIdentity: "github.com/example/kwt",
				Kind:            "branch",
				Ref:             "feature/gone",
				Branch:          "feature/gone",
				Observations: []fleet.Observation{{
					HostID:     "host-a",
					Path:       "/repos/kwt-feature-gone",
					Head:       "eee",
					ObservedAt: observedAt,
				}},
			},
		}}, nil
	}

	rows, _, err := backend.List(context.Background())
	require.NoError(t, err)
	rows, _ = backend.MergeFleet(context.Background(), rows)

	require.Len(t, rows, 2)
	var local, remote dashboard.Row
	for _, row := range rows {
		if row.Entry != nil {
			local = row
		} else {
			remote = row
		}
	}
	require.NotNil(t, local.Entry)
	require.NotNil(t, local.Fleet)
	assert.True(t, local.Fleet.Local)
	assert.Equal(t, []string{"host-b", "local"}, local.Fleet.Hosts)
	require.NotNil(t, remote.Fleet)
	assert.Equal(t, "feature/remote", remote.Fleet.Ref)
	assert.False(t, remote.Fleet.Local)
	assert.Equal(t, []string{"host-b"}, remote.Fleet.Hosts)
	assert.Equal(t, "host-b", remote.Fleet.MaterializeHost)
	assert.Equal(t, "ddd", remote.Fleet.RemoteHead)
}

func TestTUIBackendMergeFleetRendersFleetStatusFromLocalObservations(t *testing.T) {
	observedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	headSHA := strings.Repeat("a", 40)
	staleSHA := strings.Repeat("e", 40)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Fleet:    models.FleetConfig{Enabled: true, HostID: "host-a"},
	}
	localEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "kwt",
			FullPath:   "github.com/example/kwt",
		},
		Branch:     "main",
		CommitHash: headSHA,
		Path:       "/repos/kwt",
		IsMain:     true,
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{localEntry}, nil
	}
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{
			localEntry.Path: {Path: localEntry.Path, Branch: localEntry.Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.readFleetState = func(context.Context, *models.Config) (fleet.FleetState, error) {
		return fleet.FleetState{Rows: []fleet.FleetRow{
			{
				// Hub kept a stale head and phantom dirt for this host after
				// a failed publish; host-b actually matches the on-disk head.
				ProjectIdentity: "github.com/example/kwt",
				Kind:            "branch",
				Ref:             "main",
				Branch:          "main",
				Observations: []fleet.Observation{
					{
						HostID:     "host-a",
						Path:       "/repos/kwt",
						Head:       staleSHA,
						Status:     fleet.ChangeStatus{Modified: 3},
						ObservedAt: observedAt,
					},
					{HostID: "host-b", Path: "/work/host-b/kwt", Head: headSHA, ObservedAt: observedAt},
				},
			},
			{
				// Deleted locally; only the stale self-observation is dirty.
				ProjectIdentity: "github.com/example/kwt",
				Kind:            "branch",
				Ref:             "feature/remote",
				Branch:          "feature/remote",
				Observations: []fleet.Observation{
					{
						HostID:     "host-a",
						Path:       "/repos/kwt-feature-remote",
						Head:       staleSHA,
						Status:     fleet.ChangeStatus{Modified: 2},
						ObservedAt: observedAt,
					},
					{HostID: "host-b", Path: "/work/host-b/kwt-feature-remote", Head: staleSHA, ObservedAt: observedAt},
				},
			},
		}}, nil
	}

	rows, _, err := backend.List(context.Background())
	require.NoError(t, err)
	rows, _ = backend.MergeFleet(context.Background(), rows)

	require.Len(t, rows, 2)
	var local, remote dashboard.Row
	for _, row := range rows {
		if row.Entry != nil {
			local = row
		} else {
			remote = row
		}
	}
	require.NotNil(t, local.Fleet)
	assert.Equal(t, "same", local.Fleet.Sync,
		"sync must anchor at the on-disk head, not the stale hub head for this host")
	assert.Equal(t, "clean", local.Fleet.Dirty,
		"phantom dirt from a stale self-observation must not be rendered")
	require.NotNil(t, remote.Fleet)
	assert.Equal(t, "feature/remote", remote.Fleet.Ref)
	assert.Equal(t, "clean", remote.Fleet.Dirty,
		"a stale self-observation must not dirty a remote-only row")
	assert.Equal(t, "same", remote.Fleet.Sync)
}

func TestTUIBackendMergeFleetMatchesLocalDetachedWorktreeToFleetRow(t *testing.T) {
	observedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	headSHA := strings.Repeat("a", 40)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Fleet:    models.FleetConfig{Enabled: true, HostID: "host-a"},
	}
	detachedEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "kwt",
			FullPath:   "github.com/example/kwt",
		},
		Branch:     "HEAD",
		CommitHash: headSHA,
		Path:       "/repos/kwt-detached",
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{detachedEntry}, nil
	}
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{
			detachedEntry.Path: {Path: detachedEntry.Path},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.readFleetState = func(context.Context, *models.Config) (fleet.FleetState, error) {
		return fleet.FleetState{Rows: []fleet.FleetRow{{
			ProjectIdentity: "github.com/example/kwt",
			Kind:            "detached",
			Ref:             headSHA,
			Observations: []fleet.Observation{
				{HostID: "host-a", Path: detachedEntry.Path, Head: headSHA, ObservedAt: observedAt},
				{HostID: "host-b", Path: "/work/host-b/kwt-detached", Head: headSHA, ObservedAt: observedAt},
			},
		}}}, nil
	}

	rows, _, err := backend.List(context.Background())
	require.NoError(t, err)
	rows, _ = backend.MergeFleet(context.Background(), rows)

	require.Len(t, rows, 1, "detached fleet row must merge with the local row, not duplicate it")
	require.NotNil(t, rows[0].Entry)
	require.NotNil(t, rows[0].Fleet)
	assert.True(t, rows[0].Fleet.Local)
	assert.Equal(t, []string{"host-b", "local"}, rows[0].Fleet.Hosts)
}

func TestTUIBackendListIncludesRegisteredProjectWithoutOrigin(t *testing.T) {
	repoPath := newTUITestRepo(t)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "local.example/team/service",
			Name:       "service",
			Path:       repoPath,
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverLaunchWorktrees = func(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		require.Len(t, entries, 1)
		return map[string]*models.WorktreeStatus{
			entries[0].Path: {Path: entries[0].Path, Branch: entries[0].Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, samePath(repoPath, rowPathForHandoff(rows[0])))
	require.NotNil(t, rows[0].Entry.RepositoryInfo)
	assert.Equal(t, "local.example/team/service", rows[0].Entry.RepositoryInfo.FullPath)
	assert.Equal(t, "service", rows[0].Entry.RepositoryInfo.Repository)
}

func TestTUIBackendListPrefersRegisteredIdentityForGlobalLocalOnlyDuplicate(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "service")
	require.NoError(t, os.MkdirAll(repoPath, 0755))
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "local.example/team/service",
			Name:       "service",
			Path:       repoPath,
		}},
	}
	globalEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: repositoryInfoFromRootPath(repoPath),
		Branch:         "main",
		Path:           repoPath,
		IsMain:         true,
	}
	projectEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: repositoryInfoFromRootPath(repoPath),
		Branch:         "main",
		Path:           repoPath,
		IsMain:         true,
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{globalEntry}, nil
	}
	backend.discoverProjectWorktrees = func(projectPath string) ([]*discovery.GlobalWorktreeEntry, error) {
		assert.Equal(t, repoPath, projectPath)
		return []*discovery.GlobalWorktreeEntry{projectEntry}, nil
	}
	backend.discoverLaunchWorktrees = func(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		require.Len(t, entries, 1)
		require.NotNil(t, entries[0].RepositoryInfo)
		assert.Equal(t, "local.example/team/service", entries[0].RepositoryInfo.FullPath)
		return map[string]*models.WorktreeStatus{
			entries[0].Path: {Path: entries[0].Path, Branch: entries[0].Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Entry.RepositoryInfo)
	assert.Equal(t, "local.example/team/service", rows[0].Entry.RepositoryInfo.FullPath)
	assert.Equal(t, "service", rows[0].Entry.RepositoryInfo.Repository)
}

func TestTUIBackendListRegistersLaunchRepositoryBestEffort(t *testing.T) {
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "other",
			FullPath:   "github.com/example/tools",
		},
		Branch: "main",
		Path:   "/repos/other",
		IsMain: true,
	}
	var registered []models.Project
	backend := newTUIBackendWithLaunchDir(cfg, "/repos/other")
	backend.discoverGlobalWorktrees = func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverProjectWorktrees = func(projectPath string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverLaunchWorktrees = func(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{launchEntry}, nil
	}
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project)
		return errors.New("read-only config")
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{
			launchEntry.Path: {Path: launchEntry.Path, Branch: launchEntry.Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, registered, 1)
	assert.Equal(t, "github.com/example/tools", registered[0].Repository)
	assert.Equal(t, "other", registered[0].Name)
	assert.Equal(t, "/repos/other", registered[0].Path)
}

func TestTUIBackendRegistersLaunchRepositoryOnceAcrossStagedLoad(t *testing.T) {
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host: "github.com", Owner: "example", Repository: "kwt",
		},
		Branch: "main",
		Path:   "/repos/kwt",
		IsMain: true,
	}
	backend := newTUIBackendWithLaunchDir(cfg, launchEntry.Path)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{launchEntry}, nil
	}
	registrations := 0
	backend.registerProject = func(context.Context, models.Project) error {
		registrations++
		return nil
	}
	backend.collectStatuses = func(
		context.Context,
		string,
		[]*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	_, _, err := backend.ListFast(context.Background())
	require.NoError(t, err)
	_, _, err = backend.List(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, registrations)
}

func TestTUIBackendListAddsLaunchRepositoryToInMemoryProjects(t *testing.T) {
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "other",
			FullPath:   "github.com/example/tools",
		},
		Branch: "main",
		Path:   "/repos/other",
		IsMain: true,
	}
	backend := newTUIBackendWithLaunchDir(cfg, "/repos/other")
	backend.discoverGlobalWorktrees = func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverProjectWorktrees = func(projectPath string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverLaunchWorktrees = func(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{launchEntry}, nil
	}
	backend.registerProject = func(_ context.Context, project models.Project) error {
		return nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{
			launchEntry.Path: {Path: launchEntry.Path, Branch: launchEntry.Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, "github.com/example/tools", cfg.Projects[0].Repository)
	assert.Equal(t, "other", cfg.Projects[0].Name)
	assert.Equal(t, "/repos/other", cfg.Projects[0].Path)
}

func TestTUIBackendLaunchRegistrationReusesExistingProjectByPath(t *testing.T) {
	repoPath := newTUITestRepo(t)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "local.example/team/service",
			Name:       "service",
			Path:       repoPath,
		}},
	}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: repositoryInfoFromRootPath(repoPath),
		Branch:         "main",
		Path:           repoPath,
		IsMain:         true,
	}
	var registered []models.Project
	backend := newTUIBackendWithLaunchDir(cfg, repoPath)
	backend.discoverGlobalWorktrees = func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverProjectWorktrees = func(projectPath string) ([]*discovery.GlobalWorktreeEntry, error) {
		return nil, nil
	}
	backend.discoverLaunchWorktrees = func(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{launchEntry}, nil
	}
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project)
		return nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return map[string]*models.WorktreeStatus{
			launchEntry.Path: {Path: launchEntry.Path, Branch: launchEntry.Branch},
		}, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions

	_, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, registered, 1)
	assert.Equal(t, "local.example/team/service", registered[0].Repository)
	assert.Equal(t, "service", registered[0].Name)
	assert.True(t, samePath(repoPath, registered[0].Path))
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, "local.example/team/service", cfg.Projects[0].Repository)
}

func TestTUIBackendLaunchRegistrationUpgradesPathFallbackToRemoteIdentity(t *testing.T) {
	repoPath := newTUITestRepo(t)
	pathFallback := repositoryInfoFromRootPath(repoPath).FullPath
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: pathFallback,
			Name:       filepath.Base(repoPath),
			Path:       repoPath,
		}},
	}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryURL: "https://github.com/example/service-api.git",
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "service",
			FullPath:   "github.com/example/service-api",
		},
		Branch: "main",
		Path:   repoPath,
		IsMain: true,
	}
	var registered []models.Project
	backend := newTUIBackendWithLaunchDir(cfg, repoPath)
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project)
		return nil
	}

	backend.registerLaunchProject(context.Background(), []*discovery.GlobalWorktreeEntry{launchEntry})

	require.Len(t, registered, 1)
	assert.Equal(t, "github.com/example/service-api", registered[0].Repository)
	assert.Equal(t, "service", registered[0].Name)
	assert.True(t, samePath(repoPath, registered[0].Path))
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, "github.com/example/service-api", cfg.Projects[0].Repository)
}

func TestTUIBackendLaunchRegistrationKeepsConfiguredIdentityOverForkOrigin(t *testing.T) {
	repoPath := newTUITestRepo(t)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "github.com/kenn-io/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryURL: "https://github.com/fork/service-api.git",
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "fork",
			Repository: "service-api",
			FullPath:   "github.com/fork/service-api",
		},
		Branch: "main",
		Path:   repoPath,
		IsMain: true,
	}
	var registered []models.Project
	backend := newTUIBackendWithLaunchDir(cfg, repoPath)
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project)
		return nil
	}

	backend.registerLaunchProject(context.Background(), []*discovery.GlobalWorktreeEntry{launchEntry})

	require.Len(t, registered, 1)
	assert.Equal(t, "github.com/kenn-io/service-api", registered[0].Repository,
		"a stable configured identity must not be replaced by a fork origin")
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, "github.com/kenn-io/service-api", cfg.Projects[0].Repository)
}

func TestApplyProjectIdentityFallbackPrefersConfiguredIdentityOverForkOrigin(t *testing.T) {
	forkInfo, err := url.ParseRepositoryURL("https://github.com/fork/kwt.git")
	require.NoError(t, err)
	entries := []*discovery.GlobalWorktreeEntry{{
		RepositoryURL:  "https://github.com/fork/kwt.git",
		RepositoryInfo: forkInfo,
		Branch:         "main",
		Path:           "/repos/kwt",
	}}

	entries = applyProjectIdentityFallback(entries, models.Project{
		Repository: "github.com/kenn-io/kwt",
		Name:       "kwt",
		Path:       "/repos/kwt",
	})

	require.NotNil(t, entries[0].RepositoryInfo)
	assert.Equal(t, "github.com/kenn-io/kwt", entries[0].RepositoryInfo.FullPath,
		"manifest publishing keys rows by project.Repository, so discovery must too")
}

func TestApplyProjectIdentityFallbackKeepsOriginForLocalPathLikeIdentity(t *testing.T) {
	forkInfo, err := url.ParseRepositoryURL("https://github.com/fork/kwt.git")
	require.NoError(t, err)
	entries := []*discovery.GlobalWorktreeEntry{{
		RepositoryURL:  "https://github.com/fork/kwt.git",
		RepositoryInfo: forkInfo,
		Branch:         "main",
		Path:           "/repos/kwt",
	}}

	entries = applyProjectIdentityFallback(entries, models.Project{
		Repository: "local/Users/test/kwt",
		Name:       "kwt",
		Path:       "/repos/kwt",
	})

	require.NotNil(t, entries[0].RepositoryInfo)
	assert.Equal(t, "github.com/fork/kwt", entries[0].RepositoryInfo.FullPath,
		"identities that never reach the hub must not displace the origin identity that does")
}

// TestRepositoryInfoFromProjectMatchesCanonicalResolverForLocalIdentity pins
// the fix for a canonical "local/..." project identity being re-parsed as a
// URL (host="local", owner=first path segment) instead of reconstructed
// through the canonical local-path resolver: the two produced different
// WorkspaceSessionName values for the same worktree, so the TUI missed
// CLI-created sessions and created duplicates.
func TestRepositoryInfoFromProjectMatchesCanonicalResolverForLocalIdentity(t *testing.T) {
	repoPath := newTUITestRepo(t)
	canonical, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)

	project := models.Project{
		Repository: canonical.FullPath,
		Name:       canonical.Repository,
		Path:       repoPath,
	}

	info := repositoryInfoFromProject(project)

	require.NotNil(t, info)
	assert.Equal(t,
		tmux.WorkspaceSessionName(canonical, "main", repoPath),
		tmux.WorkspaceSessionName(info, "main", repoPath),
		"a local/... project identity must resolve to the same session name the canonical resolver produces")
}

func TestRepositoryInfoFromProjectMatchesCanonicalResolverForLegacyAbsoluteIdentity(t *testing.T) {
	repoPath := newTUITestRepo(t)
	canonical, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)

	info := repositoryInfoFromProject(models.Project{
		Repository: repoPath,
		Name:       canonical.Repository,
		Path:       repoPath,
	})

	require.NotNil(t, info)
	assert.Equal(t, canonical.FullPath, info.FullPath)
	assert.Equal(t,
		tmux.WorkspaceSessionName(canonical, "main", repoPath),
		tmux.WorkspaceSessionName(info, "main", repoPath),
		"an absolute legacy identity must use the canonical local resolver")
}

func TestRepositoryInfoFromProjectPreservesCanonicalNetworkAuthority(t *testing.T) {
	tests := []string{
		"host.example:2222/org/repo",
		"[2001:db8::1]:2222/org/repo",
	}
	for _, identity := range tests {
		t.Run(identity, func(t *testing.T) {
			canonical, ok := url.CanonicalRepositoryInfo(identity)
			require.True(t, ok)

			info := repositoryInfoFromProject(models.Project{
				Repository: identity,
				Name:       "repo",
				Path:       "/repos/repo",
			})

			require.NotNil(t, info)
			assert.Equal(t, canonical.FullPath, info.FullPath)
			assert.Equal(t,
				tmux.WorkspaceSessionName(canonical, "main", "/repos/repo"),
				tmux.WorkspaceSessionName(info, "main", "/repos/repo"),
				"configured authority identities must produce the canonical session name")
		})
	}
}

func TestTUIBackendLaunchRegistrationUpgradesLocalPathLikeIdentityToOrigin(t *testing.T) {
	repoPath := newTUITestRepo(t)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "local/Users/test/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryURL: "https://github.com/example/service-api.git",
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "service-api",
			FullPath:   "github.com/example/service-api",
		},
		Branch: "main",
		Path:   repoPath,
		IsMain: true,
	}
	var registered []models.Project
	backend := newTUIBackendWithLaunchDir(cfg, repoPath)
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project)
		return nil
	}

	backend.registerLaunchProject(context.Background(), []*discovery.GlobalWorktreeEntry{launchEntry})

	require.Len(t, registered, 1)
	assert.Equal(t, "github.com/example/service-api", registered[0].Repository,
		"local/... identities never reach the hub and should upgrade to the origin identity")
}

// TestTUIBackendLaunchRegistrationRejectsRelativeRemoteIdentity pins the
// provenance gate at the registration site: a relative dotless remote
// ("cache/team/repo.git" is a machine-local filesystem path git happily
// serves) must not be persisted as project.Repository, because stored
// registry identities ride the relaxed configured bar on every later
// manifest build. The local-path fallback is persisted instead.
func TestTUIBackendLaunchRegistrationRejectsRelativeRemoteIdentity(t *testing.T) {
	repoPath := newTUITestRepo(t)
	relativeInfo, err := url.ParseRepositoryURL("cache/team/repo.git")
	require.NoError(t, err)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
	}
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryURL:  "cache/team/repo.git",
		RepositoryInfo: relativeInfo,
		Branch:         "main",
		Path:           repoPath,
		IsMain:         true,
	}
	var registered []models.Project
	backend := newTUIBackendWithLaunchDir(cfg, repoPath)
	backend.registerProject = func(_ context.Context, project models.Project) error {
		registered = append(registered, project)
		return nil
	}

	backend.registerLaunchProject(context.Background(), []*discovery.GlobalWorktreeEntry{launchEntry})

	localIdentity := repositoryInfoFromRootPath(repoPath)
	require.NotNil(t, localIdentity)
	require.Len(t, registered, 1)
	assert.Equal(t, localIdentity.FullPath, registered[0].Repository,
		"a relative dotless remote must persist as the local fallback, not a shareable identity")
	require.Len(t, cfg.Projects, 1)
	assert.Equal(t, localIdentity.FullPath, cfg.Projects[0].Repository)
}

// TestTUIBackendAutoRegisteredRelativeRemoteNeverReachesManifest characterizes
// the laundering end-to-end: auto-register a repo whose real origin is a
// relative dotless remote (via the same discovery the TUI runs), then build a
// fleet manifest from the resulting registry and require that the bogus
// "cache/..." identity is never published.
func TestTUIBackendAutoRegisteredRelativeRemoteNeverReachesManifest(t *testing.T) {
	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "cache/team/repo.git")

	launchEntries, err := discoverLaunchRepoWorktrees(repoPath)
	require.NoError(t, err)
	require.NotEmpty(t, launchEntries)

	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
	}
	backend := newTUIBackendWithLaunchDir(cfg, repoPath)
	backend.registerProject = func(context.Context, models.Project) error { return nil }

	backend.registerLaunchProject(context.Background(), launchEntries)
	require.Len(t, cfg.Projects, 1)

	builder := fleet.NewManifestBuilder(fleet.ManifestBuilderOptions{
		Hostname: func() (string, error) { return "host-a", nil },
		ListProjectWorktrees: func(context.Context, models.Project) ([]models.Worktree, error) {
			return nil, nil
		},
		DiscoverGlobalWorktrees: func(
			string, []models.Project,
		) ([]*discovery.GlobalWorktreeEntry, error) {
			return nil, nil
		},
	})
	manifest, err := builder.Build(context.Background(), cfg)
	require.NoError(t, err)
	for _, project := range manifest.Projects {
		assert.NotEqual(t, "cache/team/repo", project.Identity,
			"a git-derived relative remote must never launder into a published fleet identity")
	}
}

func TestApplyProjectIdentityFallbackKeepsOriginForPathBackedProjects(t *testing.T) {
	forkInfo, err := url.ParseRepositoryURL("https://github.com/fork/kwt.git")
	require.NoError(t, err)
	entries := []*discovery.GlobalWorktreeEntry{{
		RepositoryURL:  "https://github.com/fork/kwt.git",
		RepositoryInfo: forkInfo,
		Branch:         "main",
		Path:           "/repos/kwt",
	}}

	entries = applyProjectIdentityFallback(entries, models.Project{
		Repository: "/repos/kwt",
		Name:       "kwt",
		Path:       "/repos/kwt",
	})

	require.NotNil(t, entries[0].RepositoryInfo)
	assert.Equal(t, "github.com/fork/kwt", entries[0].RepositoryInfo.FullPath,
		"a path-backed configured identity must not displace a real remote identity")
}

func TestHasStableProjectIdentityRejectsAbsolutePathFallbacks(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		want       bool
	}{
		{name: "remote full path", repository: "github.com/example/service-api", want: true},
		// local/... is a path fallback: it never reaches the hub, so an
		// origin-derived identity may replace it (see reusableExistingProject).
		{name: "path safe local identity", repository: "local/Users/test/service", want: false},
		{name: "relative project identity", repository: "workspace/service", want: true},
		{name: "unix absolute path", repository: "/Users/test/service", want: false},
		{name: "unix absolute tmp path", repository: "/var/tmp/service", want: false},
		{name: "windows slash absolute path", repository: `C:/Users/test/service`, want: false},
		{name: "windows backslash absolute path", repository: `C:\Users\test\service`, want: false},
		{name: "windows unc absolute path", repository: `\\server\share\service`, want: false},
		{name: "slash unc absolute path", repository: "//server/share/service", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasStableProjectIdentity(models.Project{Repository: tt.repository})

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTUIBackendRemoveWorktreeFallsBackToRegisteredProjectRoot(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "stale-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/stale", worktreePath)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	require.NoError(t, os.RemoveAll(worktreePath))

	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "github.com/example/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		RepositoryURL: "https://github.com/example/service-api.git",
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "service-api",
			FullPath:   "github.com/example/service-api",
		},
		Branch:     "codex/stale",
		Path:       worktreePath,
		Generation: generation,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	useInProcessTUIRemoval(t, backend)

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	output := runTUITestGitOutput(t, repoPath, "worktree", "list", "--porcelain")
	assert.NotContains(t, output, worktreePath)
}

func TestTUIBackendRemoveWorktreeDelegatesToDaemon(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "daemon-tui-remove")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "daemon-tui-remove", worktreePath)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	head := strings.TrimSpace(runTUITestGitOutput(t, worktreePath, "rev-parse", "HEAD"))
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Path: worktreePath, Branch: "daemon-tui-remove", CommitHash: head, Generation: generation,
	}, SessionName: "workspace"}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.liveEndpoints = func(
		context.Context,
		tmux.WorkspaceEndpointRequest,
	) ([]tmux.SessionEndpoint, error) {
		return nil, nil
	}
	var request kwt.RemovalRequest
	backend.removeWorktree = func(
		_ context.Context,
		input kwt.RemovalRequest,
	) (kwt.RemovalResult, error) {
		request = input
		return kwt.RemovalResult{
			Path: input.Path, Branch: "daemon-tui-remove", WorktreeRemoved: true,
		}, nil
	}

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	assert.Equal(t, utils.PathKey(repoPath), utils.PathKey(request.RepositoryPath))
	assert.Equal(t, utils.PathKey(worktreePath), utils.PathKey(request.Path))
	assert.Equal(t, generation, request.ExpectedGeneration)
	assert.Equal(t, "daemon-tui-remove", request.ExpectedBranch)
	assert.Equal(t, head, request.ExpectedHead)
	assert.DirExists(t, worktreePath, "only the daemon service may perform the mutation")
}

func TestTUIBackendRemoveWorktreeDoesNotGateMutationOnCleanupInspection(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "cleanup-inspection-remove")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "cleanup-inspection-remove", worktreePath)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	row := dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{
			Path: worktreePath, Branch: "cleanup-inspection-remove", Generation: generation,
		},
		SessionName: "kwt-wt-widget-cleanup-01234567",
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	removed := false
	backend.removeWorktree = func(
		_ context.Context,
		request kwt.RemovalRequest,
	) (kwt.RemovalResult, error) {
		removed = true
		return kwt.RemovalResult{Path: request.Path, WorktreeRemoved: true}, nil
	}
	backend.liveEndpoints = func(
		context.Context,
		tmux.WorkspaceEndpointRequest,
	) ([]tmux.SessionEndpoint, error) {
		return nil, errors.New("tmux inventory unavailable")
	}

	err := backend.RemoveWorktree(context.Background(), row, false)

	assert.True(t, removed)
	require.Error(t, err)
	assert.ErrorContains(t, err, "tmux inventory unavailable")
	assert.True(t, git.WorktreeWasRemoved(err),
		"the TUI must refresh after removal even when cleanup inspection fails")
}

func TestTUIBackendDashboardKeepsRowsWhenOneSessionIsUnsafe(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.resolveSessions = bestEffortDashboardSessionResolver(func(
		context.Context,
		[]tmux.WorkspaceEndpointRequest,
	) ([]tmux.WorkspaceSessionResolution, error) {
		return []tmux.WorkspaceSessionResolution{
			{
				Session: tmux.WorkspaceSession{Endpoint: testCanonicalSessionEndpoint("stale")},
				Err:     errors.New("session belongs to a different worktree generation"),
			},
			{
				Session: tmux.WorkspaceSession{
					Endpoint: testCanonicalSessionEndpoint("live"),
					Live:     true,
				},
			},
		}, nil
	}, nil)
	entries := []*discovery.GlobalWorktreeEntry{
		{Path: "/work/stale", TmuxEndpoint: tmux.SessionEndpoint{SessionName: "stale"}},
		{Path: "/work/live", TmuxEndpoint: tmux.SessionEndpoint{SessionName: "live"}},
	}

	sessions, _, err := backend.resolveDashboardSessions(context.Background(), entries, nil)

	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, testCanonicalSessionEndpoint("stale"), sessions[0].Endpoint)
	assert.False(t, sessions[0].Live)
	assert.Equal(t, testCanonicalSessionEndpoint("live"), sessions[1].Endpoint)
	assert.True(t, sessions[1].Live)
}

func TestTUIBackendRemoveWorktreeKillsEveryMatchingEndpointAfterRemoval(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "duplicate-session-remove")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "duplicate-session-remove", worktreePath)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	sessionName := "kwt-wt-widget-duplicate-01234567"
	row := dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{
			Path: worktreePath, Branch: "duplicate-session-remove", Generation: generation,
		},
		SessionName:  sessionName,
		SessionLive:  true,
		TmuxEndpoint: testCanonicalSessionEndpoint(sessionName),
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	removed := false
	backend.liveEndpoints = func(
		_ context.Context,
		request tmux.WorkspaceEndpointRequest,
	) ([]tmux.SessionEndpoint, error) {
		assert.True(t, removed, "cleanup discovery must not gate removal")
		assert.Equal(t, sessionName, request.SessionName)
		assert.Equal(t, worktreePath, request.WorkspacePath)
		assert.Equal(t, generation, request.WorkspaceGeneration)
		return []tmux.SessionEndpoint{
			testCanonicalSessionEndpoint(sessionName),
			{SessionName: sessionName},
		}, nil
	}
	backend.removeWorktree = func(
		_ context.Context,
		request kwt.RemovalRequest,
	) (kwt.RemovalResult, error) {
		removed = true
		return kwt.RemovalResult{Path: request.Path, WorktreeRemoved: true}, nil
	}
	var killed []tmux.SessionEndpoint
	backend.cleanupEndpoint = func(
		_ context.Context,
		endpoint tmux.SessionEndpoint,
		request tmux.WorkspaceEndpointRequest,
	) error {
		assert.True(t, removed, "sessions must remain live until removal succeeds")
		assert.Equal(t, worktreePath, request.WorkspacePath)
		assert.Equal(t, generation, request.WorkspaceGeneration)
		killed = append(killed, endpoint)
		return nil
	}
	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	assert.Equal(t, []tmux.SessionEndpoint{
		testCanonicalSessionEndpoint(sessionName),
		{SessionName: sessionName},
	}, killed)
}

func TestTUIBackendRemoveWorktreeSweepsSessionsCreatedDuringRemoval(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "racing-session-remove")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "racing-session-remove", worktreePath)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	sessionName := "kwt-wt-widget-racing-01234567"
	lateSessionName := "kwt-wt-widget-previous-89abcdef"
	row := dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{
			Path: worktreePath, Branch: "racing-session-remove", Generation: generation,
		},
		SessionName:  sessionName,
		SessionLive:  true,
		TmuxEndpoint: testCanonicalSessionEndpoint(sessionName),
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	removed := false
	inspections := 0
	backend.liveEndpoints = func(
		_ context.Context,
		request tmux.WorkspaceEndpointRequest,
	) ([]tmux.SessionEndpoint, error) {
		inspections++
		assert.Equal(t, sessionName, request.SessionName)
		assert.Equal(t, worktreePath, request.WorkspacePath)
		assert.Equal(t, generation, request.WorkspaceGeneration)
		if !removed {
			return []tmux.SessionEndpoint{testCanonicalSessionEndpoint(sessionName)}, nil
		}
		return []tmux.SessionEndpoint{
			testCanonicalSessionEndpoint(sessionName),
			{SessionName: lateSessionName},
		}, nil
	}
	backend.removeWorktree = func(
		_ context.Context,
		request kwt.RemovalRequest,
	) (kwt.RemovalResult, error) {
		removed = true
		return kwt.RemovalResult{Path: request.Path, WorktreeRemoved: true}, nil
	}
	var killed []tmux.SessionEndpoint
	backend.cleanupEndpoint = func(
		_ context.Context,
		endpoint tmux.SessionEndpoint,
		_ tmux.WorkspaceEndpointRequest,
	) error {
		killed = append(killed, endpoint)
		return nil
	}

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	assert.Equal(t, 1, inspections)
	assert.Equal(t, []tmux.SessionEndpoint{
		testCanonicalSessionEndpoint(sessionName),
		{SessionName: lateSessionName},
	}, killed)
}

func TestTUIBackendRemoveWorktreeCleansProtectedEndpointWithoutSharedLiveness(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "protected-session-remove")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "protected-session-remove", worktreePath)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	protectedEndpoint := tmux.SessionEndpoint{
		SessionName: "kwt-wt-widget-protected-01234567",
		SocketName:  "kwt-pr-protected-01234567",
	}
	row := dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{
			Path:         worktreePath,
			Branch:       "protected-session-remove",
			Generation:   generation,
			Protected:    true,
			TmuxEndpoint: protectedEndpoint,
		},
		SessionName:  protectedEndpoint.SessionName,
		SessionLive:  false,
		TmuxEndpoint: protectedEndpoint,
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.liveEndpoints = func(
		context.Context,
		tmux.WorkspaceEndpointRequest,
	) ([]tmux.SessionEndpoint, error) {
		return nil, nil
	}
	removed := false
	backend.removeWorktree = func(
		_ context.Context,
		request kwt.RemovalRequest,
	) (kwt.RemovalResult, error) {
		removed = true
		return kwt.RemovalResult{Path: request.Path, WorktreeRemoved: true}, nil
	}
	backend.cleanupEndpoint = func(
		_ context.Context,
		endpoint tmux.SessionEndpoint,
		_ tmux.WorkspaceEndpointRequest,
	) error {
		t.Fatalf("protected endpoint routed through ordinary cleanup: %+v", endpoint)
		return nil
	}
	var killed []tmux.SessionEndpoint
	backend.killProtectedEndpoint = func(
		_ context.Context,
		endpoint tmux.SessionEndpoint,
		request tmux.WorkspaceEndpointRequest,
	) error {
		assert.True(t, removed)
		assert.Equal(t, worktreePath, request.WorkspacePath)
		assert.Equal(t, generation, request.WorkspaceGeneration)
		killed = append(killed, endpoint)
		return nil
	}

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	assert.Equal(t, []tmux.SessionEndpoint{protectedEndpoint}, killed)
}

func TestTUIBackendProtectedCleanupIgnoresMissingTmuxAfterRemoval(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "missing"))
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")

	err := backend.killProtectedTUIEndpoint(
		context.Background(),
		tmux.SessionEndpoint{
			SessionName: "kwt-wt-widget-protected-01234567",
			SocketName:  "kwt-pr-protected-01234567",
		},
		tmux.WorkspaceEndpointRequest{
			SessionName:         "kwt-wt-widget-protected-01234567",
			WorkspacePath:       "/work/widget",
			WorkspaceGeneration: "0123456789abcdef0123456789abcdef",
		},
	)

	require.NoError(t, err)
}

func TestTUIBackendProtectedCleanupTerminatesCanonicalAndLegacySessions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is unavailable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	ctx := context.Background()
	tempDir, err := os.MkdirTemp("/tmp", "kwt-tui-cleanup-")
	require.NoError(t, err)
	t.Setenv("TMUX_TMPDIR", tempDir)
	socketName := fmt.Sprintf("kwt-pr-cleanup-%d", time.Now().UnixNano())
	sessionName := "kwt-wt-widget-protected-01234567"
	generation := "0123456789abcdef0123456789abcdef"
	canonical := tmux.NewTmuxCommandForSocketWithStripNames(
		"tmux", socketName, nil,
	)
	legacy := tmux.NewTmuxCommandForSocketInTempDirWithStripNames(
		"tmux", socketName, tempDir, nil,
	)
	t.Cleanup(func() {
		_ = canonical.RunCommandContext(ctx, "kill-server")
		_ = legacy.RunCommandContext(ctx, "kill-server")
		require.NoError(t, os.RemoveAll(tempDir))
	})
	for _, command := range []*tmux.TmuxCommand{canonical, legacy} {
		require.NoError(t, command.RunCommandContext(
			ctx, "new-session", "-d", "-s", sessionName, "sleep", "60",
		))
		require.NoError(t, command.SetOptionContext(
			ctx, sessionName, "@kwt-workspace-generation", generation,
		))
		require.NoError(t, command.SetOptionContext(
			ctx, sessionName, "@kwt-cleanup-"+generation, "1",
		))
	}

	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	err = backend.killProtectedTUIEndpoint(
		ctx,
		tmux.SessionEndpoint{
			SessionName: sessionName,
			SocketName:  socketName,
		},
		tmux.WorkspaceEndpointRequest{
			SessionName:         sessionName,
			WorkspaceGeneration: generation,
		},
	)

	require.NoError(t, err)
	assert.False(t, canonical.HasSession(sessionName))
	assert.False(t, legacy.HasSession(sessionName))
}

func TestTUIBackendRemoveWorktreeSkipsUnresolvedProtectedEndpoint(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "unresolved-protected-remove")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "unresolved-protected-remove", worktreePath)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	row := dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{
			Path:       worktreePath,
			Branch:     "unresolved-protected-remove",
			Generation: generation,
			Protected:  true,
			TmuxEndpoint: tmux.SessionEndpoint{
				SessionName: "kwt-wt-widget-unverified-01234567",
			},
		},
		SessionName: "kwt-wt-widget-unverified-01234567",
		TmuxEndpoint: tmux.SessionEndpoint{
			SessionName: "kwt-wt-widget-unverified-01234567",
		},
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.liveEndpoints = func(
		context.Context,
		tmux.WorkspaceEndpointRequest,
	) ([]tmux.SessionEndpoint, error) {
		return nil, nil
	}
	backend.removeWorktree = func(
		_ context.Context,
		request kwt.RemovalRequest,
	) (kwt.RemovalResult, error) {
		return kwt.RemovalResult{Path: request.Path, WorktreeRemoved: true}, nil
	}
	backend.cleanupEndpoint = func(
		_ context.Context,
		endpoint tmux.SessionEndpoint,
		_ tmux.WorkspaceEndpointRequest,
	) error {
		t.Fatalf("unresolved protected row routed through ordinary cleanup: %+v", endpoint)
		return nil
	}
	backend.killProtectedEndpoint = func(
		_ context.Context,
		endpoint tmux.SessionEndpoint,
		_ tmux.WorkspaceEndpointRequest,
	) error {
		t.Fatalf("unresolved protected row routed through protected cleanup: %+v", endpoint)
		return nil
	}

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
}

func useInProcessTUIRemoval(t *testing.T, backend *tuiBackend) {
	t.Helper()
	home, err := config.CanonicalHome()
	require.NoError(t, err)
	backend.liveEndpoints = func(
		context.Context,
		tmux.WorkspaceEndpointRequest,
	) ([]tmux.SessionEndpoint, error) {
		return nil, nil
	}
	backend.removeWorktree = kwt.NewRemovalService(
		kwt.RemovalServiceOptions{Home: home},
	).Remove
}

func allowTUIProjectOperations(backend *tuiBackend) {
	backend.runProjectOperation = func(
		_ context.Context,
		_ string,
		_ bool,
		_ []string,
		mutation func() error,
	) error {
		return mutation()
	}
}

func TestTUIBackendCreateWorktreePublishesAfterSuccessfulMutation(t *testing.T) {
	resetFleetCommandDeps(t)
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	cfg := &models.Config{
		Fleet:    models.FleetConfig{Enabled: true},
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "worktrees"), AutoMkdir: true},
	}
	published := 0
	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		return &stubFleetManifestBuilder{}
	}
	publishFleetBestEffort = func(ctx context.Context, gotCfg *models.Config, builder fleet.ManifestBuildProvider, warn *bytes.Buffer) error {
		published++
		assert.Equal(t, cfg, gotCfg)
		assert.NotNil(t, builder)
		assert.NotNil(t, warn)
		return errors.New("hub unavailable")
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch: "main",
		Path:   repoPath,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	backend.loadTargetConfig = func(string, bool) (*models.Config, error) {
		return cfg, nil
	}

	path, err := backend.CreateWorktree(context.Background(), row, "feature/from-tui", "")

	require.NoError(t, err)
	assert.DirExists(t, path)
	assert.Equal(t, 1, published)
}

func TestTUIBackendCreateWorktreeLosesToProjectRemoval(t *testing.T) {
	resetFleetCommandDeps(t)
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repoPath := newTUITestRepo(t)
	require.NoError(t, config.RegisterProject(models.Project{
		Repository: "github.com/example/kwt", Name: "kwt", Path: repoPath,
	}))
	cfg := &models.Config{Worktree: models.WorktreeConfig{
		BaseDir: filepath.Join(t.TempDir(), "worktrees"), AutoMkdir: true,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	backend.loadTargetConfig = func(string, bool) (*models.Config, error) { return cfg, nil }
	oldBeforeAcquire := beforeProjectGuardAcquire
	t.Cleanup(func() { beforeProjectGuardAcquire = oldBeforeAcquire })
	beforeProjectGuardAcquire = func() {
		snapshot, snapshotErr := config.LoadGlobalSnapshotAt(home)
		require.NoError(t, snapshotErr)
		changed, removeErr := config.CompareAndSwapProjectAt(home, snapshot.Projects[0], nil)
		require.NoError(t, removeErr)
		require.True(t, changed)
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{Branch: "main", Path: repoPath}}

	path, err := backend.CreateWorktree(
		context.Background(), row, "feature/removed-before-create", "",
	)

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.Empty(t, path)
}

func TestTUIBackendExistingLocalBranchRemainsUnreviewed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "checkout", "-b", "feature/local")
	runTUITestGit(t, repoPath, "checkout", "main")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   filepath.Join(t.TempDir(), "worktrees"),
			AutoMkdir: true,
		},
		Naming: models.NamingConfig{
			Template: "{{.Branch}}",
			SanitizeChars: map[string]string{
				"/": "-",
			},
		},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch: "main",
		Path:   repoPath,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	allowTUIProjectOperations(backend)
	backend.loadTargetConfig = func(string, bool) (*models.Config, error) {
		return cfg, nil
	}

	path, err := backend.CreateWorktree(
		context.Background(),
		row,
		"feature/local",
		"feature/local",
	)

	require.NoError(t, err)
	reg, err := registry.New()
	require.NoError(t, err)
	assert.True(t, reg.IsUnreviewedRemoteSource(path))
}

func TestTUIBackendWorktreeCreationUsesSelectedRepositoryConfig(t *testing.T) {
	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	globalBase := filepath.Join(t.TempDir(), "global-worktrees")
	selectedBase := filepath.Join(t.TempDir(), "selected-worktrees")
	globalCfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: globalBase, AutoMkdir: true},
	}
	selectedCfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: selectedBase, AutoMkdir: true},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch: "main",
		Path:   repoPath,
	}}
	backend := newTUIBackendWithLaunchDir(globalCfg, "")
	allowTUIProjectOperations(backend)
	backend.loadTargetConfig = func(path string, interactive bool) (*models.Config, error) {
		expectedRoot, err := filepath.EvalSymlinks(repoPath)
		require.NoError(t, err)
		assert.Equal(t, expectedRoot, path)
		assert.False(t, interactive)
		return selectedCfg, nil
	}

	planned, err := backend.PreviewWorktree(row, "feature/selected-config")
	require.NoError(t, err)
	path, err := backend.CreateWorktree(
		context.Background(),
		row,
		"feature/selected-config",
		"",
	)

	require.NoError(t, err)
	assert.Equal(t, planned.Entry.Path, path)
	assert.True(t, strings.HasPrefix(path, selectedBase+string(os.PathSeparator)))
	assert.False(t, strings.HasPrefix(path, globalBase+string(os.PathSeparator)))
}

func TestTUIBackendCreateWorktreeDoesNotExpandRepositoryLocalTemplate(t *testing.T) {
	const secret = "credential-must-not-appear-in-path"
	t.Setenv("KWT_GITHUB_TOKEN", secret)

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "worktrees"), AutoMkdir: true},
		Naming: models.NamingConfig{
			Template:                `{{printf "%c%s" 36 "KWT_GITHUB_TOKEN"}}/{{.Branch}}`,
			TemplateRepositoryLocal: true,
		},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch: "main",
		Path:   repoPath,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	allowTUIProjectOperations(backend)
	backend.loadTargetConfig = func(string, bool) (*models.Config, error) {
		return cfg, nil
	}

	planned, err := backend.PreviewWorktree(row, "feature/from-tui")
	require.NoError(t, err)
	path, err := backend.CreateWorktree(context.Background(), row, "feature/from-tui", "")

	require.NoError(t, err)
	assert.NotContains(t, path, secret,
		"a repository-generated name must not have its environment references expanded")
	assert.Contains(t, path, "$KWT_GITHUB_TOKEN")
	assert.Equal(t, planned.Entry.Path, path,
		"the optimistic row's path must match where creation lands")
	assert.DirExists(t, path)
}

func TestTUIBackendRemoveWorktreePublishesAfterSuccessfulMutation(t *testing.T) {
	resetFleetCommandDeps(t)
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "removable-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/removable", worktreePath)
	cfg := &models.Config{
		Fleet:    models.FleetConfig{Enabled: true},
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "github.com/example/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	published := 0
	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		return &stubFleetManifestBuilder{}
	}
	publishFleetBestEffort = func(ctx context.Context, gotCfg *models.Config, builder fleet.ManifestBuildProvider, warn *bytes.Buffer) error {
		published++
		assert.Equal(t, cfg, gotCfg)
		assert.NotNil(t, builder)
		assert.NotNil(t, warn)
		return errors.New("hub unavailable")
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "service-api",
			FullPath:   "github.com/example/service-api",
		},
		Branch: "codex/removable",
		Path:   worktreePath,
		Generation: tuiTestWorktreeGeneration(
			t,
			repoPath,
			worktreePath,
		),
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	useInProcessTUIRemoval(t, backend)

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	assert.Equal(t, 1, published)
}

func TestTUIBackendCompletesBookkeepingAfterGitDeregistersWithResidualFiles(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell wrapper")
	}
	resetFleetCommandDeps(t)
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "tui-remove-residual")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"codex/tui-remove-residual",
		worktreePath,
	)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:       worktreePath,
		Branch:     "codex/tui-remove-residual",
		Generation: generation,
	}))

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	wrapper := `#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "remove" ]; then
	"$REAL_GIT" "$@" || exit $?
	mkdir -p "$3"
	printf 'created during removal\n' > "$3/residual"
	printf "error: failed to delete '%s': Directory not empty\n" "$3" >&2
	exit 1
fi
exec "$REAL_GIT" "$@"
`
	require.NoError(t, os.WriteFile(wrapperPath, []byte(wrapper), 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(wrapperDir, "tmux"),
		[]byte("#!/bin/sh\nexit 0\n"),
		0755,
	))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &models.Config{
		Fleet: models.FleetConfig{Enabled: true},
		Projects: []models.Project{{
			Repository: "github.com/example/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	published := 0
	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		return &stubFleetManifestBuilder{}
	}
	publishFleetBestEffort = func(
		context.Context,
		*models.Config,
		fleet.ManifestBuildProvider,
		*bytes.Buffer,
	) error {
		published++
		return nil
	}
	row := dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{
			RepositoryInfo: &url.RepositoryInfo{
				Host:       "github.com",
				Owner:      "example",
				Repository: "service-api",
				FullPath:   "github.com/example/service-api",
			},
			Branch:     "codex/tui-remove-residual",
			Path:       worktreePath,
			Generation: generation,
		},
		SessionLive: true,
		SessionName: "kwt-workspace-service-api-residual",
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	useInProcessTUIRemoval(t, backend)

	err = backend.RemoveWorktree(context.Background(), row, false)

	require.ErrorContains(t, err, "worktree removed, but files remain at ")
	assert.True(t, git.WorktreeWasRemoved(err))
	assert.Equal(t, 1, published)
	assert.FileExists(t, filepath.Join(worktreePath, "residual"))
	refreshedRegistry, registryErr := registry.New()
	require.NoError(t, registryErr)
	_, registered := refreshedRegistry.Get(worktreePath)
	assert.False(t, registered)
}

func TestTUIBackendRemoveWorktreeUnregistersLegacyEntry(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "legacy-registry-worktree")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"codex/legacy-registry",
		worktreePath,
	)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:                   worktreePath,
		Branch:                 "codex/legacy-registry",
		UnreviewedRemoteSource: true,
	}))
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch: "codex/legacy-registry",
		Path:   worktreePath,
		Generation: tuiTestWorktreeGeneration(
			t,
			repoPath,
			worktreePath,
		),
	}}
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
	}, "")
	useInProcessTUIRemoval(t, backend)

	err = backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	refreshedRegistry, err := registry.New()
	require.NoError(t, err)
	_, registered := refreshedRegistry.Get(worktreePath)
	assert.False(t, registered)
}

func TestTUIBackendRemoveWorktreeRejectsReplacementGeneration(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "replacement-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/original", worktreePath)
	originalHead := strings.TrimSpace(runTUITestGitOutput(t, worktreePath, "rev-parse", "HEAD"))
	worktrees, err := git.New(repoPath).ListWorktrees()
	require.NoError(t, err)
	var originalGeneration string
	for _, worktree := range worktrees {
		if utils.CanonicalPath(worktree.Path) == utils.CanonicalPath(worktreePath) {
			originalGeneration = worktree.Generation
		}
	}
	require.NotEmpty(t, originalGeneration)

	runTUITestGit(t, repoPath, "worktree", "remove", worktreePath)
	runTUITestGit(t, repoPath, "branch", "-D", "codex/original")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/replacement", worktreePath)
	replacementGeneration := tuiTestWorktreeGeneration(
		t,
		repoPath,
		worktreePath,
	)
	require.NotEqual(t, originalGeneration, replacementGeneration)
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch:     "codex/original",
		CommitHash: originalHead,
		Path:       worktreePath,
		Generation: originalGeneration,
	}}
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
	}, "")
	useInProcessTUIRemoval(t, backend)

	err = backend.RemoveWorktree(context.Background(), row, true)

	require.ErrorContains(t, err, "generation changed")
	var refreshRequired interface{ RefreshRequired() bool }
	require.ErrorAs(t, err, &refreshRequired)
	assert.True(t, refreshRequired.RefreshRequired())
	assert.DirExists(t, worktreePath)
}

func TestTUIBackendRemoveWorktreeChangedHeadRequiresRefresh(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "changed-head-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/changed-head", worktreePath)
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch:     "codex/changed-head",
		CommitHash: strings.TrimSpace(runTUITestGitOutput(t, worktreePath, "rev-parse", "HEAD")),
		Path:       worktreePath,
		Generation: tuiTestWorktreeGeneration(t, repoPath, worktreePath),
	}}
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "new.txt"), []byte("new\n"), 0644))
	runTUITestGit(t, worktreePath, "add", "new.txt")
	runTUITestGit(t, worktreePath, "commit", "-m", "advance checkout")
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Worktree: models.WorktreeConfig{BaseDir: t.TempDir()},
	}, "")
	useInProcessTUIRemoval(t, backend)

	err := backend.RemoveWorktree(context.Background(), row, true)

	require.Error(t, err)
	var refreshRequired interface{ RefreshRequired() bool }
	require.ErrorAs(t, err, &refreshRequired)
	assert.True(t, refreshRequired.RefreshRequired())
	assert.DirExists(t, worktreePath)
}

func TestTUIBackendRejectsRepositoryRootFromDifferentIdentity(t *testing.T) {
	repoA := newTUITestRepo(t)
	repoB := newTUITestRepo(t)
	runTUITestGit(
		t,
		repoA,
		"remote",
		"add",
		"origin",
		"https://github.com/example/repo-a.git",
	)
	runTUITestGit(
		t,
		repoB,
		"remote",
		"add",
		"origin",
		"https://github.com/example/repo-b.git",
	)
	repoAInfo, err := url.ParseRepositoryURL(
		"https://github.com/example/repo-a.git",
	)
	require.NoError(t, err)
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Projects: []models.Project{{
			Repository: "github.com/example/repo-a",
			Path:       repoA,
		}},
	}, "")
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Path:           repoB,
		RepositoryInfo: repoAInfo,
	}}

	_, err = backend.repositoryRootForRow(row)

	require.Error(t, err)
	assert.ErrorContains(t, err, "repository identity changed")
}

func TestTUIBackendAcceptsDiscoveryLocalRepositoryIdentity(t *testing.T) {
	tests := []struct {
		name   string
		origin func(*testing.T, string)
	}{
		{name: "without origin"},
		{
			name: "filesystem origin",
			origin: func(t *testing.T, repoPath string) {
				runTUITestGit(
					t,
					repoPath,
					"remote",
					"add",
					"origin",
					filepath.Join(t.TempDir(), "upstream.git"),
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoPath := newTUITestRepo(t)
			if tt.origin != nil {
				tt.origin(t, repoPath)
			}
			repoInfo, err := worktree.RepositoryInfoFromLocalPath(repoPath)
			require.NoError(t, err)
			backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
			row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
				Path:           repoPath,
				RepositoryInfo: repoInfo,
			}}

			root, err := backend.repositoryRootForRow(row)

			require.NoError(t, err)
			assert.True(t, samePath(repoPath, root))
		})
	}
}

func TestTUIBackendRemoveWorktreeDirtyErrorDoesNotSuggestCLIForce(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "dirty-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/dirty", worktreePath)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty\n"), 0644))

	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "github.com/example/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "service-api",
			FullPath:   "github.com/example/service-api",
		},
		Branch: "codex/dirty",
		Path:   worktreePath,
		Generation: tuiTestWorktreeGeneration(
			t,
			repoPath,
			worktreePath,
		),
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	useInProcessTUIRemoval(t, backend)

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "uncommitted changes")
	assert.NotContains(t, err.Error(), "kwt remove --force")
	var refreshRequired interface{ RefreshRequired() bool }
	require.ErrorAs(t, err, &refreshRequired)
	assert.True(t, refreshRequired.RefreshRequired())
	assert.DirExists(t, worktreePath)
}

func TestTUIBackendForceRemoveDeletesDirtyWorktree(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "dirty-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/dirty", worktreePath)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty\n"), 0644))

	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "github.com/example/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "service-api",
			FullPath:   "github.com/example/service-api",
		},
		Branch:     "codex/dirty",
		CommitHash: strings.TrimSpace(runTUITestGitOutput(t, worktreePath, "rev-parse", "HEAD")),
		Path:       worktreePath,
		Generation: tuiTestWorktreeGeneration(
			t,
			repoPath,
			worktreePath,
		),
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	useInProcessTUIRemoval(t, backend)

	err := backend.RemoveWorktree(context.Background(), row, true)

	require.NoError(t, err)
	assert.NoDirExists(t, worktreePath)
}

func TestTUIBackendMaterializeWorktreeUsesRegisteredProjectRoot(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	runTUITestGit(t, repoPath, "branch", "feature/studio-only")
	baseDir := filepath.Join(t.TempDir(), "worktrees")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir, AutoMkdir: true},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       repoPath,
		}},
	}
	require.NoError(t, config.RegisterProject(cfg.Projects[0]))
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUITargetConfig(backend, cfg)
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/studio-only",
		Branch:          "feature/studio-only",
		Hosts:           []string{"host-b"},
	}}

	path, err := backend.MaterializeWorktree(context.Background(), row)

	require.NoError(t, err)
	assert.DirExists(t, path)
	assert.True(t, strings.HasPrefix(path, baseDir), path)
	branch := strings.TrimSpace(runTUITestGitOutput(t, path, "branch", "--show-current"))
	assert.Equal(t, "feature/studio-only", branch)
}

func TestTUIBackendMaterializeWorktreeRequiresCurrentRegistration(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "branch", "feature/removed-project")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir: filepath.Join(t.TempDir(), "worktrees"), AutoMkdir: true,
		},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt", Name: "kwt", Path: repoPath,
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUITargetConfig(backend, cfg)
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/removed-project",
		Branch:          "feature/removed-project",
		Hosts:           []string{"host-b"},
	}}

	path, err := backend.MaterializeWorktree(context.Background(), row)

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.Empty(t, path)
}

func TestTUIBackendMaterializeWorktreeUsesSelectedProjectConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "branch", "feature/selected-config")
	globalBase := filepath.Join(t.TempDir(), "global-worktrees")
	selectedBase := filepath.Join(t.TempDir(), "selected-worktrees")
	globalCfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   globalBase,
			AutoMkdir: true,
		},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       repoPath,
		}},
	}
	selectedCfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir:   selectedBase,
			AutoMkdir: true,
		},
		Naming: models.NamingConfig{
			Template: "{{.Branch}}",
			SanitizeChars: map[string]string{
				"/": "-",
			},
		},
	}
	backend := newTUIBackendWithLaunchDir(globalCfg, "")
	allowTUIProjectOperations(backend)
	loadedTargetConfig := false
	backend.loadTargetConfig = func(path string, interactive bool) (*models.Config, error) {
		loadedTargetConfig = true
		assert.Equal(t, repoPath, path)
		assert.False(t, interactive)
		return selectedCfg, nil
	}
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/selected-config",
		Branch:          "feature/selected-config",
		Hosts:           []string{"host-b"},
	}}

	path, err := backend.MaterializeWorktree(context.Background(), row)

	require.NoError(t, err)
	assert.True(t, loadedTargetConfig)
	assert.True(t, strings.HasPrefix(path, selectedBase+string(os.PathSeparator)))
	assert.False(t, strings.HasPrefix(path, globalBase+string(os.PathSeparator)))
}

func TestTUIBackendMaterializeWorktreeTracksRemoteOnlyBranch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(
		t,
		repoPath,
		"remote",
		"add",
		"origin",
		"https://github.com/example/kwt.git",
	)
	runTUITestGit(
		t,
		repoPath,
		"update-ref",
		"refs/remotes/origin/feature/remote-only",
		"HEAD",
	)
	baseDir := filepath.Join(t.TempDir(), "worktrees")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir, AutoMkdir: true},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       repoPath,
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	allowTUIProjectOperations(backend)
	stubTUITargetConfig(backend, cfg)
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/remote-only",
		Branch:          "feature/remote-only",
		Hosts:           []string{"host-b"},
	}}

	path, err := backend.MaterializeWorktree(context.Background(), row)

	require.NoError(t, err)
	assert.DirExists(t, path)
	assert.Equal(
		t,
		"feature/remote-only",
		strings.TrimSpace(runTUITestGitOutput(
			t,
			path,
			"branch",
			"--show-current",
		)),
	)
	assert.Equal(
		t,
		"origin/feature/remote-only",
		strings.TrimSpace(runTUITestGitOutput(
			t,
			path,
			"rev-parse",
			"--abbrev-ref",
			"@{upstream}",
		)),
	)
}

func TestTUIBackendMaterializeWorktreeRejectsStaleLocalHead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	runTUITestGit(t, repoPath, "branch", "feature/studio-only")
	baseDir := filepath.Join(t.TempDir(), "worktrees")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir, AutoMkdir: true},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       repoPath,
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	allowTUIProjectOperations(backend)
	stubTUITargetConfig(backend, cfg)
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/studio-only",
		Branch:          "feature/studio-only",
		Hosts:           []string{"host-b"},
		RemoteHead:      strings.Repeat("b", 40),
	}}

	path, err := backend.MaterializeWorktree(context.Background(), row)

	require.Error(t, err)
	assert.Empty(t, path)
	assert.Contains(t, err.Error(), "reported head")
	assert.Contains(t, err.Error(), "push or fetch")
	assert.NoDirExists(t, filepath.Join(baseDir, "github.com", "example", "kwt", "feature-studio-only"))
	assert.True(t, tuiTestBranchExists(repoPath, "feature/studio-only"),
		"pre-existing branch must survive a failed sync")
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	assert.False(t, reg.IsUnreviewedRemoteSource(
		filepath.Join(baseDir, "github.com", "example", "kwt", "feature-studio-only"),
	))
}

func TestTUIBackendMaterializeWorktreeDeletesAutoCreatedBranchOnStaleHead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	// Simulate a fetched remote branch with no local counterpart, so
	// `git worktree add` auto-creates the local branch.
	runTUITestGit(t, repoPath, "update-ref", "refs/remotes/origin/feature/studio-only", "HEAD")
	rollbackHookMarker := filepath.Join(t.TempDir(), "rollback-hook-ran")
	hooksDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(hooksDir, "reference-transaction"),
		fmt.Appendf(
			nil,
			"#!/bin/sh\nprintf hook > %q\n",
			rollbackHookMarker,
		),
		0755,
	))
	runTUITestGit(t, repoPath, "config", "core.hooksPath", hooksDir)
	baseDir := filepath.Join(t.TempDir(), "worktrees")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir, AutoMkdir: true},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       repoPath,
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	allowTUIProjectOperations(backend)
	stubTUITargetConfig(backend, cfg)
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/studio-only",
		Branch:          "feature/studio-only",
		Hosts:           []string{"host-b"},
		RemoteHead:      strings.Repeat("b", 40),
	}}

	path, err := backend.MaterializeWorktree(context.Background(), row)

	require.Error(t, err)
	assert.Empty(t, path)
	assert.False(t, tuiTestBranchExists(repoPath, "feature/studio-only"),
		"auto-created branch must be removed when verification fails")
	assert.NoFileExists(t, rollbackHookMarker,
		"fleet branch rollback must not run repository hooks")
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	assert.False(t, reg.IsUnreviewedRemoteSource(
		filepath.Join(baseDir, "github.com", "example", "kwt", "feature-studio-only"),
	))
}

func TestTUIBackendMaterializeWorktreeDeletesAutoCreatedBranchOnCheckoutFailure(
	t *testing.T,
) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(
		t,
		repoPath,
		"remote",
		"add",
		"origin",
		"https://github.com/example/kwt.git",
	)
	runTUITestGit(t, repoPath, "checkout", "-b", "feature/broken-remote")
	require.NoError(t, os.WriteFile(
		filepath.Join(repoPath, "missing.txt"),
		[]byte("unique missing blob\n"),
		0644,
	))
	runTUITestGit(t, repoPath, "add", "missing.txt")
	runTUITestGit(t, repoPath, "commit", "-m", "Add missing blob")
	commit := strings.TrimSpace(runTUITestGitOutput(
		t,
		repoPath,
		"rev-parse",
		"HEAD",
	))
	blob := strings.TrimSpace(runTUITestGitOutput(
		t,
		repoPath,
		"rev-parse",
		"HEAD:missing.txt",
	))
	runTUITestGit(t, repoPath, "checkout", "main")
	runTUITestGit(
		t,
		repoPath,
		"update-ref",
		"refs/remotes/origin/feature/broken-remote",
		commit,
	)
	runTUITestGit(t, repoPath, "branch", "-D", "feature/broken-remote")
	require.NoError(t, os.Remove(filepath.Join(
		repoPath,
		".git",
		"objects",
		blob[:2],
		blob[2:],
	)))
	rollbackHookMarker := filepath.Join(t.TempDir(), "rollback-hook-ran")
	hooksDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(hooksDir, "reference-transaction"),
		fmt.Appendf(
			nil,
			"#!/bin/sh\nprintf hook > %q\n",
			rollbackHookMarker,
		),
		0755,
	))
	runTUITestGit(t, repoPath, "config", "core.hooksPath", hooksDir)

	baseDir := filepath.Join(t.TempDir(), "worktrees")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir, AutoMkdir: true},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       repoPath,
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	allowTUIProjectOperations(backend)
	stubTUITargetConfig(backend, cfg)
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/broken-remote",
		Branch:          "feature/broken-remote",
		Hosts:           []string{"host-b"},
	}}

	path, err := backend.MaterializeWorktree(context.Background(), row)

	require.Error(t, err)
	assert.Empty(t, path)
	assert.False(t, tuiTestBranchExists(repoPath, "feature/broken-remote"),
		"auto-created branch must be removed when checkout fails")
	assert.NoFileExists(t, rollbackHookMarker,
		"fleet branch rollback must not run repository hooks")
}

func TestTUIBackendMaterializeWorktreeReportsBranchRollbackFailure(t *testing.T) {
	repoPath := newTUITestRepo(t)
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")

	err := backend.rollbackMaterializedBranch(
		git.New(repoPath),
		"missing-branch",
		errors.New("materialization failed"),
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "materialization failed")
	assert.ErrorContains(t, err, "failed to remove auto-created branch")
	assert.ErrorContains(t, err, "an incomplete worktree may remain")
}

func TestTUIBackendMaterializeWorktreeUnregistersWhenHeadCannotBeRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	runTUITestGit(t, repoPath, "branch", "feature/studio-only")
	baseDir := filepath.Join(t.TempDir(), "worktrees")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir, AutoMkdir: true},
	}
	manager := worktree.New(git.New(repoPath), cfg)
	worktreePath, err := manager.AddWithOptions(
		"feature/studio-only",
		"",
		false,
		worktree.AddOptions{SkipSetup: true},
	)
	require.NoError(t, err)
	runTUITestGit(t, worktreePath, "symbolic-ref", "HEAD", "refs/heads/missing")

	backend := newTUIBackendWithLaunchDir(cfg, "")
	err = backend.verifyMaterializedHead(
		context.Background(),
		repoPath,
		worktreePath,
		tuiTestWorktreeGeneration(t, repoPath, worktreePath),
		&dashboard.FleetInfo{
			Branch:     "feature/studio-only",
			RemoteHead: strings.Repeat("b", 40),
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not verify synced head")
	assert.NoDirExists(t, worktreePath)
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	assert.False(t, reg.IsUnreviewedRemoteSource(worktreePath))
}

func TestTUIBackendMaterializationCleanupPreservesReplacementWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "materialized")
	runTUITestGit(t, repoPath, "branch", "feature/materialized")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		worktreePath,
		"feature/materialized",
	)
	originalGeneration := tuiTestWorktreeGeneration(
		t,
		repoPath,
		worktreePath,
	)
	runTUITestGit(t, repoPath, "worktree", "remove", "--force", worktreePath)
	runTUITestGit(t, repoPath, "branch", "feature/replacement")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		worktreePath,
		"feature/replacement",
	)

	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	err := backend.failMaterializedHeadVerification(
		repoPath,
		worktreePath,
		originalGeneration,
		errors.New("stale materialized head"),
	)

	require.Error(t, err)
	assert.DirExists(t, worktreePath)
	assert.Equal(
		t,
		"feature/replacement",
		strings.TrimSpace(runTUITestGitOutput(
			t,
			worktreePath,
			"rev-parse",
			"--abbrev-ref",
			"HEAD",
		)),
	)
}

func tuiTestBranchExists(repoPath string, branch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoPath
	return cmd.Run() == nil
}

func stubTUITargetConfig(backend *tuiBackend, cfg *models.Config) {
	backend.loadTargetConfig = func(string, bool) (*models.Config, error) {
		return cfg, nil
	}
}

func TestTUIBackendMaterializeWorktreeSkipsRepositorySetupCommands(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("setup command execution requires sh")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	runTUITestGit(t, repoPath, "branch", "feature/studio-only")
	baseDir := filepath.Join(t.TempDir(), "worktrees")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: baseDir, AutoMkdir: true},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       repoPath,
		}},
		RepositorySettings: []models.RepositorySetting{{
			Repository:    repoPath,
			SetupCommands: []string{"printf setup > setup-ran"},
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	allowTUIProjectOperations(backend)
	stubTUITargetConfig(backend, cfg)
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/studio-only",
		Branch:          "feature/studio-only",
		Hosts:           []string{"host-b"},
	}}

	path, err := backend.MaterializeWorktree(context.Background(), row)

	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(path, "setup-ran"))
}

func TestProjectForFleetInfoRequiresRepositoryIdentityMatch(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Projects: []models.Project{{
			Repository: "github.com/example/other",
			Name:       "kwt",
			Path:       "/repos/other",
		}},
	}, "")

	project, ok := backend.projectForFleetInfo(&dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
	})

	assert.False(t, ok)
	assert.Empty(t, project.Path)
}

func TestProjectForFleetInfoMatchesNormalizedRepositoryIdentity(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{
		Projects: []models.Project{{
			Repository: "https://github.com/example/kwt.git",
			Name:       "service",
			Path:       "/repos/kwt",
		}},
	}, "")

	project, ok := backend.projectForFleetInfo(&dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
	})

	require.True(t, ok)
	assert.Equal(t, "/repos/kwt", project.Path)
}

func TestTUIBackendMaterializeWorktreeExplainsUnavailableBranch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "worktrees"), AutoMkdir: true},
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       repoPath,
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	allowTUIProjectOperations(backend)
	stubTUITargetConfig(backend, cfg)
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "github.com/example/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/not-pushed",
		Branch:          "feature/not-pushed",
		Hosts:           []string{"host-b"},
	}}

	_, err := backend.MaterializeWorktree(context.Background(), row)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "materialize")
	assert.Contains(t, err.Error(), "branch must exist locally or on a fetched remote")
	assert.Contains(t, err.Error(), "push or fetch it first")
}

func TestTUIBackendRemoveWorktreeRejectsBrokenGitFile(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "broken-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/broken", worktreePath)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreePath, ".git"),
		[]byte("gitdir: /missing/repo/.git/worktrees/broken-worktree\n"),
		0644,
	))

	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Projects: []models.Project{{
			Repository: "github.com/example/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		RepositoryURL: "https://github.com/example/service-api.git",
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "service-api",
			FullPath:   "github.com/example/service-api",
		},
		Branch:     "codex/broken",
		Path:       worktreePath,
		Generation: generation,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	useInProcessTUIRemoval(t, backend)

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.Error(t, err)
	output := runTUITestGitOutput(t, repoPath, "worktree", "list", "--porcelain")
	assert.Contains(t, output, "branch refs/heads/codex/broken")
	assert.DirExists(t, worktreePath)
}

func TestTUIBackendResolveLayoutFallsBackToRegisteredProjectRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "broken-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/broken-layout", worktreePath)
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreePath, ".git"),
		[]byte("gitdir: /missing/repo/.git/worktrees/broken-worktree\n"),
		0644,
	))

	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "global")},
		Agents: map[string]string{
			"codex": "codex --profile kwt",
		},
		Layouts: models.LayoutsConfig{
			Default: "quad",
			Presets: []models.Layout{{
				Name:    "quad",
				Arrange: "tiled",
				Panes:   []string{"agent:codex"},
			}},
		},
		Projects: []models.Project{{
			Repository: "github.com/example/service-api",
			Name:       "service-api",
			Path:       repoPath,
		}},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		RepositoryURL: "https://github.com/example/service-api.git",
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "example",
			Repository: "service-api",
			FullPath:   "github.com/example/service-api",
		},
		Branch: "codex/broken-layout",
		Path:   worktreePath,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	layout, err := backend.resolveLayout(row, "", false)

	require.NoError(t, err)
	assert.Equal(t, []string{"codex --profile kwt"}, layout.Panes)
}

func TestProjectMatchesRowRejectsDuplicateBasenameIdentityMismatch(t *testing.T) {
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{
			Host:       "github.com",
			Owner:      "org-two",
			Repository: "service",
			FullPath:   "github.com/org-two/service",
		},
	}}

	assert.False(t, projectMatchesRow(models.Project{
		Repository: "github.com/org-one/service",
		Name:       "service",
		Path:       "/repos/org-one/service",
	}, row))
	assert.True(t, projectMatchesRow(models.Project{
		Repository: "github.com/org-two/service",
		Name:       "service",
		Path:       "/repos/org-two/service",
	}, row))
}

func TestDiscoverLaunchRepoWorktreesListsLocalOnlyRepository(t *testing.T) {
	repoPath := newTUITestRepo(t)

	entries, err := discoverLaunchRepoWorktrees(repoPath)

	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, samePath(repoPath, entries[0].Path))
	assert.Equal(t, "main", entries[0].Branch)
	assert.True(t, entries[0].IsMain)
	assert.Empty(t, entries[0].RepositoryURL)
	require.NotNil(t, entries[0].RepositoryInfo)
	// A no-remote repository resolves through the single canonical resolver to
	// the "local/..." identity, matching kwt list and kwt list -g discovery.
	wantInfo, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)
	assert.Equal(t, wantInfo.FullPath, entries[0].RepositoryInfo.FullPath)
	assert.Equal(t, filepath.Base(repoPath), entries[0].RepositoryInfo.Repository)
}

func TestTUIBackendRemovesLaunchWorktreeOutsideGlobalBase(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "outside-global-base")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"feature/outside",
		worktreePath,
	)
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{
			BaseDir: filepath.Join(t.TempDir(), "global"),
		},
	}
	backend := newTUIBackendWithLaunchDir(cfg, repoPath)
	useInProcessTUIRemoval(t, backend)
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.registerProject = func(context.Context, models.Project) error { return nil }
	backend.registerWorkspace = func(
		workspace models.Workspace,
	) (models.Workspace, error) {
		return workspace, nil
	}

	rows, _, err := backend.ListFast(context.Background())
	require.NoError(t, err)
	var row dashboard.Row
	for _, candidate := range rows {
		if candidate.Entry != nil &&
			samePath(candidate.Entry.Path, worktreePath) {
			row = candidate
			break
		}
	}
	require.NotNil(t, row.Entry)
	require.NotEmpty(t, row.Entry.Generation)

	err = backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	assert.NoDirExists(t, worktreePath)
}

// TestDiscoverLaunchRepoWorktreesRejectsRelativeDotlessRemote pins the
// remote-provenance gate on launch discovery: a relative dotless filesystem
// remote must not surface as a shareable identity. The entry retains the raw
// URL only as provenance for registry revalidation while carrying the same
// local/... identity the shared resolver reports.
func TestDiscoverLaunchRepoWorktreesRejectsRelativeDotlessRemote(t *testing.T) {
	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "cache/team/repo.git")

	entries, err := discoverLaunchRepoWorktrees(repoPath)

	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "cache/team/repo.git", entries[0].RepositoryURL,
		"the raw barred remote is retained only for configured-identity revalidation")
	wantInfo, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)
	require.NotNil(t, entries[0].RepositoryInfo)
	assert.Equal(t, wantInfo.FullPath, entries[0].RepositoryInfo.FullPath)
}

func newTUITestRepo(t *testing.T) string {
	t.Helper()

	repoPath := filepath.Join(t.TempDir(), "repo")
	runTUITestGit(t, "", "init", "-b", "main", repoPath)
	runTUITestGit(t, repoPath, "config", "user.name", "Test User")
	runTUITestGit(t, repoPath, "config", "user.email", "test@example.com")

	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("# Test Repository\n"), 0644))
	runTUITestGit(t, repoPath, "add", ".")
	runTUITestGit(t, repoPath, "commit", "-m", "Initial commit")
	return repoPath
}

func runTUITestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = runTUITestGitOutput(t, dir, args...)
}

func runTUITestGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, fmt.Sprintf("git %s failed:\n%s", strings.Join(args, " "), output))
	return string(output)
}

func tuiTestWorktreeGeneration(
	t *testing.T,
	repoPath string,
	worktreePath string,
) string {
	t.Helper()
	worktrees, err := git.New(repoPath).ListWorktrees()
	require.NoError(t, err)
	for _, worktree := range worktrees {
		if utils.CanonicalPath(worktree.Path) ==
			utils.CanonicalPath(worktreePath) {
			require.NotEmpty(t, worktree.Generation)
			return worktree.Generation
		}
	}
	t.Fatalf("worktree %s not found", worktreePath)
	return ""
}

func stubTUIProjectRegistration(backend *tuiBackend) {
	backend.registerProject = func(context.Context, models.Project) error { return nil }
	backend.registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		return workspace, nil
	}
}

func TestTUIBackendMergeFleetReturnsHubWarnings(t *testing.T) {
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Fleet:    models.FleetConfig{Enabled: true, HostID: "host-a"},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.readFleetState = func(context.Context, *models.Config) (fleet.FleetState, error) {
		return fleet.FleetState{Warnings: []fleet.Warning{{
			Code:    "host_id_collision",
			HostID:  "same",
			Message: "multiple machines are publishing as host ID \"same\"",
		}}}, nil
	}

	rows, _, err := backend.List(context.Background())
	require.NoError(t, err)
	_, warnings := backend.MergeFleet(context.Background(), rows)

	require.Len(t, warnings, 1)
	assert.Equal(t, `multiple machines are publishing as host ID "same" (host same)`, warnings[0])
}

func TestTUIBackendListIncludesWorkspaceRows(t *testing.T) {
	dir := t.TempDir()
	cfg := &models.Config{
		Worktree:   models.WorktreeConfig{BaseDir: "/global"},
		Workspaces: []models.Workspace{{Name: "notes", Path: dir}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	liveSession := tmux.DirWorkspaceSessionName("old-name", dir)
	backend.resolveSessions = func(
		_ context.Context,
		requests []tmux.WorkspaceEndpointRequest,
	) ([]tmux.WorkspaceSession, error) {
		require.Len(t, requests, 1)
		return []tmux.WorkspaceSession{{
			Endpoint: testCanonicalSessionEndpoint(liveSession),
			Live:     true,
		}}, nil
	}

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Workspace)
	assert.Equal(t, "notes", rows[0].Workspace.Name)
	assert.Equal(t, dir, rows[0].Workspace.Path)
	assert.True(t, rows[0].SessionLive,
		"liveness must match by path hash even under an old session name")
	assert.Equal(t, liveSession, rows[0].SessionName,
		"attach must target the live session, not the freshly computed name")
	assert.Nil(t, rows[0].Entry)
	assert.Nil(t, rows[0].Fleet)
}

func TestTUIBackendAutoRegistersNonGitLaunchDir(t *testing.T) {
	launchDir := t.TempDir()
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	backend := newTUIBackendWithLaunchDir(cfg, launchDir)
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	var registered []models.Workspace
	backend.registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		registered = append(registered, workspace)
		return workspace, nil
	}

	_, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, registered, 1)
	assert.Equal(t, launchDir, registered[0].Path)
}

func TestTUIBackendSkipsAutoRegisterWhenAlreadyRegisteredWithCustomName(t *testing.T) {
	launchDir := t.TempDir()
	cfg := &models.Config{
		Worktree:   models.WorktreeConfig{BaseDir: "/global"},
		Workspaces: []models.Workspace{{Name: "mynotes", Path: launchDir}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, launchDir)
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	called := false
	backend.registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		called = true
		return workspace, nil
	}

	_, _, err := backend.List(context.Background())

	require.NoError(t, err)
	assert.False(t, called, "an already-registered launch dir must not be re-registered")
	require.Len(t, cfg.Workspaces, 1)
	assert.Equal(t, "mynotes", cfg.Workspaces[0].Name,
		"a custom workspace name must survive the auto-registration refresh")
}

func TestTUIBackendAutoRegistersLaunchWorkspaceOnlyOnce(t *testing.T) {
	launchDir := t.TempDir()
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	backend := newTUIBackendWithLaunchDir(cfg, launchDir)
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	registrations := 0
	backend.registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		registrations++
		return workspace, nil
	}
	backend.unregisterWorkspace = func(name string) error { return nil }

	_, _, err := backend.List(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, registrations)

	require.NoError(t, backend.UnregisterWorkspace(dashboard.Row{
		Workspace: &dashboard.WorkspaceInfo{Name: filepath.Base(launchDir), Path: launchDir},
	}))
	assert.Empty(t, cfg.Workspaces, "unregister must drop the in-memory entry")

	_, _, err = backend.List(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, registrations,
		"a second refresh must not re-register the launch workspace after it was unregistered")
	assert.Empty(t, cfg.Workspaces,
		"the unregistered launch workspace must not reappear on the next refresh")
}

func TestTUIBackendNeverRegistersWorkspaceForGitLaunchDir(t *testing.T) {
	launchDir := "/repos/other"
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	backend := newTUIBackendWithLaunchDir(cfg, launchDir)
	stubTUIProjectRegistration(backend)
	launchEntry := &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{Host: "github.com", Owner: "example", Repository: "other"},
		Branch:         "main",
		Path:           launchDir,
		IsMain:         true,
	}
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) {
		return []*discovery.GlobalWorktreeEntry{launchEntry}, nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		t.Fatalf("a git launch directory must never be registered as a workspace, got %v", workspace)
		return workspace, nil
	}

	_, _, err := backend.List(context.Background())

	require.NoError(t, err)
	assert.Empty(t, cfg.Workspaces)
}

func TestTUIBackendNeverAutoRegistersHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
	cfg := &models.Config{Worktree: models.WorktreeConfig{BaseDir: "/global"}}
	backend := newTUIBackendWithLaunchDir(cfg, home)
	stubTUIProjectRegistration(backend)
	backend.discoverGlobalWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverProjectWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverLaunchWorktrees = func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		return nil, nil, nil
	}
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.registerWorkspace = func(workspace models.Workspace) (models.Workspace, error) {
		t.Fatalf("home directory must never be auto-registered, got %v", workspace)
		return workspace, nil
	}

	_, _, err := backend.List(context.Background())

	require.NoError(t, err)
}

func TestTUIBackendSessionNameAndHandoffPathForWorkspaceRow(t *testing.T) {
	row := dashboard.Row{
		Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: "/Users/me/notes"},
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")

	name, err := backend.sessionName(row)

	require.NoError(t, err)
	assert.Equal(t, tmux.DirWorkspaceSessionName("notes", "/Users/me/notes"), name,
		"an unset SessionName must fall back to the workspace branch")
	assert.Equal(t, "/Users/me/notes", rowPathForHandoff(row))

	row.SessionName = "live-session-name"
	name, err = backend.sessionName(row)

	require.NoError(t, err)
	assert.Equal(t, tmux.DirWorkspaceSessionName("notes", "/Users/me/notes"), name,
		"directory workspace establishment must always request the canonical name")
}

func TestRowPaneRoot(t *testing.T) {
	assert.Equal(t, "/Users/me/notes", rowPaneRoot(dashboard.Row{
		Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: "/Users/me/notes"},
	}), "a workspace row must use the workspace path")

	assert.Equal(t, "/repos/service", rowPaneRoot(dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{Path: "/repos/service"},
	}), "an entry row must use the entry path")

	assert.Equal(t, "", rowPaneRoot(dashboard.Row{}),
		"an empty row must fall back to an empty pane root")
}

func TestTUIBackendLayoutNamesPrependsNone(t *testing.T) {
	backend := newTUIBackend(&models.Config{
		Layouts: models.LayoutsConfig{
			Presets: []models.Layout{{Name: "quad", Arrange: "tiled", Panes: []string{""}}},
		},
	})
	assert.Equal(t, []string{"none", "quad"}, backend.LayoutNames())
}

func TestTUIBackendLayoutNamesBlankOnly(t *testing.T) {
	backend := newTUIBackend(&models.Config{})
	assert.Equal(t, []string{"none"}, backend.LayoutNames())
}

func TestTUIBackendCarriesConfiguredCredentialName(t *testing.T) {
	backend := newTUIBackend(&models.Config{
		Fleet: models.FleetConfig{TokenEnv: "Custom_Fleet_Token"},
	})

	assert.ElementsMatch(
		t,
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN", "Custom_Fleet_Token"},
		backend.protectedNames,
	)
}

func TestTUIBackendAttachWorkspaceGuardRejectsEmptyRow(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")

	err := backend.attachWorkspace(context.Background(), dashboard.Row{}, "", false)

	require.Error(t, err)
	assert.Equal(t, "no worktree selected", err.Error())
}

func TestTUIKillSessionUsesContextMatchedEndpoint(t *testing.T) {
	want := tmux.SessionEndpoint{
		SessionName: "workspace",
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	var killed tmux.SessionEndpoint
	var request tmux.WorkspaceEndpointRequest
	backend.cleanupEndpoint = func(
		_ context.Context,
		endpoint tmux.SessionEndpoint,
		got tmux.WorkspaceEndpointRequest,
	) error {
		killed = endpoint
		request = got
		return nil
	}
	err := backend.KillSession(dashboard.Row{
		Entry: &discovery.GlobalWorktreeEntry{
			Path:       "/work/widget",
			Generation: "11111111111111111111111111111111",
		},
		SessionName:  "workspace",
		SessionLive:  true,
		TmuxEndpoint: want,
	})

	require.NoError(t, err)
	assert.Equal(t, want, killed)
	assert.Equal(t, tmux.WorkspaceEndpointRequest{
		SessionName:         "workspace",
		WorkspacePath:       "/work/widget",
		WorkspaceGeneration: "11111111111111111111111111111111",
	}, request)
}

func TestDashboardInventoryEntryClassifiesProtectionFromWireMode(t *testing.T) {
	for _, test := range []struct {
		name       string
		socketName string
		attachMode models.TmuxAttachMode
		protected  bool
	}{
		{
			name:       "protected mode with ordinary-looking socket",
			socketName: tmux.KWTServerSocketName,
			attachMode: models.TmuxAttachProtected,
			protected:  true,
		},
		{
			name:       "direct mode with protected-looking socket",
			socketName: "kwt-pr-not-authoritative",
			attachMode: models.TmuxAttachDirect,
			protected:  false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := dashboardInventoryEntry(kwt.Entry{
				SessionName:    "workspace",
				TmuxSocketName: test.socketName,
				TmuxAttachMode: test.attachMode,
			})

			assert.Equal(t, test.protected, entry.Protected)
			assert.Equal(t, test.socketName, entry.TmuxEndpoint.SocketName)
		})
	}
}

func TestDashboardInventoryRowRetainsAdoptedEndpoint(t *testing.T) {
	entry := kwt.Entry{
		Path:           "/work/widget",
		SessionName:    "workspace",
		TmuxAttachMode: models.TmuxAttachDirect,
	}

	row := buildTUIRow(
		dashboardInventoryEntry(entry),
		&models.WorktreeStatus{},
		tmux.WorkspaceSession{
			Endpoint: tmux.SessionEndpoint{
				SessionName: "workspace",
			},
			Live: true,
		},
	)

	assert.Empty(t, row.TmuxEndpoint.SocketName)
	assert.True(t, row.SessionLive)
}

func TestTUIWorktreeAttachCannotRaceGuardedRemoval(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "tui-open-race")
	runTUITestGit(t, repoPath, "branch", "tui-open-race")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "tui-open-race")
	generation, err := git.New(repoPath).WorktreeGeneration(worktreePath)
	require.NoError(t, err)
	initCommandTestConfig(t, t.TempDir())
	home := os.Getenv("KWT_HOME")
	file, err := os.OpenFile(filepath.Join(home, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = fmt.Fprintf(
		file,
		"\n[[projects]]\nrepository = %q\nname = 'repository'\npath = %q\n",
		repoPath,
		repoPath,
	)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	expansion, err := kwt.CaptureExpansionContext()
	require.NoError(t, err)
	removalSessionName, _, err := lifecycle.ResolveCurrentWorktreeSessionIdentity(
		context.Background(), worktreePath, nil, nil,
	)
	require.NoError(t, err)
	removalGuard := &blockingOpenRemovalGuard{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	removalDone := make(chan error, 1)
	go func() {
		_, removeErr := lifecycle.NewRemovalService(lifecycle.RemovalServiceOptions{
			Home: home, SessionGuard: removalGuard,
		}).Remove(context.Background(), lifecycle.RemovalRequest{
			RepositoryPath: repoPath,
			Path:           worktreePath, ExpectedGeneration: generation,
			Expansion: expansion,
			Session: &tmux.RemovalSessionCondition{
				SessionName: removalSessionName, Absent: true,
			},
		})
		removalDone <- removeErr
	}()
	<-removalGuard.entered

	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	ensureCalled := make(chan struct{}, 1)
	attachCalled := make(chan struct{}, 1)
	backend.ensureWorktree = func(
		_ context.Context, session string, _ string, _ string, _ models.Layout,
	) (tmux.SessionEndpoint, error) {
		ensureCalled <- struct{}{}
		return testCanonicalSessionEndpoint(session), nil
	}
	backend.attachSession = func(context.Context, tmux.SessionEndpoint) error {
		attachCalled <- struct{}{}
		return nil
	}
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- backend.attachWorkspace(
			context.Background(),
			dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
				Path: worktreePath, Branch: "tui-open-race", Generation: generation,
				RepositoryInfo: &url.RepositoryInfo{FullPath: repoPath},
			}},
			tmux.BlankLayoutName,
			false,
		)
	}()

	select {
	case <-ensureCalled:
		t.Fatal("TUI established a session during guarded removal")
	case <-time.After(100 * time.Millisecond):
	}
	close(removalGuard.release)
	require.NoError(t, <-removalDone)
	require.Error(t, <-attachDone)
	select {
	case <-ensureCalled:
		t.Fatal("TUI established a session after its worktree was removed")
	default:
	}
	select {
	case <-attachCalled:
		t.Fatal("TUI attached after its worktree was removed")
	default:
	}
}

func TestTUIWorktreeAttachUsesBranchObservedInsideLifecycleGuard(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "branch-switch-tui")
	runTUITestGit(t, repoPath, "branch", "feature/original")
	runTUITestGit(t, repoPath, "worktree", "add", worktreePath, "feature/original")
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	repositoryInfo, err := worktree.RepositoryInfoFromLocalPath(repoPath)
	require.NoError(t, err)
	initCommandTestConfig(t, t.TempDir())

	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	var ensuredSession string
	var attachedSession string
	backend.ensureWorktree = func(
		_ context.Context, session string, _, _ string, _ models.Layout,
	) (tmux.SessionEndpoint, error) {
		ensuredSession = session
		return testCanonicalSessionEndpoint(session), nil
	}
	backend.attachSession = func(_ context.Context, endpoint tmux.SessionEndpoint) error {
		attachedSession = endpoint.SessionName
		return nil
	}
	originalBeforeAcquire := beforeProjectGuardAcquire
	t.Cleanup(func() { beforeProjectGuardAcquire = originalBeforeAcquire })
	switched := false
	beforeProjectGuardAcquire = func() {
		if switched {
			return
		}
		switched = true
		runTUITestGit(t, worktreePath, "switch", "-c", "feature/current")
	}

	err = backend.attachWorkspace(
		context.Background(),
		dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
			Path: worktreePath, Branch: "feature/original", Generation: generation,
			RepositoryInfo: repositoryInfo,
		}},
		tmux.BlankLayoutName,
		false,
	)

	require.NoError(t, err)
	expected := tmux.WorkspaceSessionName(repositoryInfo, "feature/current", worktreePath)
	assert.Equal(t, expected, ensuredSession)
	assert.Equal(t, expected, attachedSession)
}

func TestTUIBackendAttachAcknowledgesRemoteSourceBeforeWorkspaceLaunch(t *testing.T) {
	workspacePath := t.TempDir()
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	var acknowledged string
	backend.acknowledgeRemoteSource = func(path string) error {
		acknowledged = path
		return nil
	}
	launched := false
	backend.ensureWorkspace = func(
		_ context.Context,
		session string,
		_ string,
		_ models.Layout,
	) (tmux.SessionEndpoint, error) {
		launched = true
		assert.Equal(t, workspacePath, acknowledged)
		return testCanonicalSessionEndpoint(session), nil
	}
	backend.attachSession = func(context.Context, tmux.SessionEndpoint) error { return nil }

	err := backend.attachWorkspace(
		context.Background(),
		dashboard.Row{Workspace: &dashboard.WorkspaceInfo{
			Name: "remote",
			Path: workspacePath,
		}},
		tmux.BlankLayoutName,
		false,
	)

	require.NoError(t, err)
	assert.True(t, launched)
	assert.Equal(t, workspacePath, acknowledged)
}

func TestTUIBackendRefusesProtectedPullRequestWorkspace(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	workspacePath := t.TempDir()
	liveGeneration := "0123456789abcdef0123456789abcdef"
	stubPRWorkspaceGeneration(t, workspacePath, liveGeneration)
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-32"] = pullrequest.Provenance{Workspace: pullrequest.Workspace{
				Path: workspacePath, Generation: liveGeneration,
			}}
			return nil
		},
	))
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	called := false
	backend.ensureWorkspace = func(
		context.Context,
		string,
		string,
		models.Layout,
	) (tmux.SessionEndpoint, error) {
		called = true
		return tmux.SessionEndpoint{}, nil
	}

	err := backend.attachWorkspace(
		context.Background(),
		dashboard.Row{
			Workspace: &dashboard.WorkspaceInfo{
				Name: "pr-32",
				Path: workspacePath,
			},
		},
		tmux.BlankLayoutName,
		false,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kwt pr attach")
	assert.False(t, called)
}

// TestTUIBackendAttachWorkspacePassesWorkspaceRowToRunner keeps the command
// layer on a stubbed runner boundary. In particular, this must never create a
// detached session on the user's default tmux server merely because a unit
// test needs to prove a workspace-only row clears the selection guard.
func TestTUIBackendAttachWorkspacePassesWorkspaceRowToRunner(t *testing.T) {
	workspacePath := t.TempDir()
	row := dashboard.Row{
		Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: workspacePath},
		SessionName: tmux.DirWorkspaceSessionName(
			"old-name", workspacePath,
		),
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	before := defaultTmuxSessions(t)

	var gotSession, gotRoot string
	var gotLayout models.Layout
	backend.ensureWorkspace = func(
		_ context.Context,
		session, root string,
		layout models.Layout,
	) (tmux.SessionEndpoint, error) {
		requireTUIBackendStateLocked(t, backend)
		gotSession, gotRoot = session, root
		gotLayout = layout
		return testCanonicalSessionEndpoint(session), nil
	}
	backend.attachSession = func(context.Context, tmux.SessionEndpoint) error { return nil }

	err := backend.AttachOutsideTmux(row, tmux.BlankLayoutName)

	require.NoError(t, err)
	assert.Equal(t, tmux.DirWorkspaceSessionName("notes", row.Workspace.Path), gotSession)
	assert.Equal(t, row.Workspace.Path, gotRoot)
	assert.Equal(t, tmux.BlankLayout(), gotLayout)
	assert.Equal(t, before, defaultTmuxSessions(t),
		"the stubbed command-layer test must not add default-server tmux sessions")
}

func TestTUIBackendOpenInTmuxPreparesResidentAttach(t *testing.T) {
	row := dashboard.Row{
		Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: t.TempDir()},
	}
	endpoint := testCanonicalSessionEndpoint("workspace")
	wantProcess := exec.Command("tmux", "attach-session", "-t", endpoint.SessionName)
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	backend.ensureWorkspace = func(
		context.Context,
		string,
		string,
		models.Layout,
	) (tmux.SessionEndpoint, error) {
		return endpoint, nil
	}
	backend.attachSession = func(context.Context, tmux.SessionEndpoint) error {
		t.Fatal("resident attach must not replace the Bubble Tea process")
		return nil
	}
	backend.prepareResidentAttach = func(
		_ context.Context,
		got tmux.SessionEndpoint,
	) (*exec.Cmd, error) {
		assert.Equal(t, endpoint, got)
		return wantProcess, nil
	}

	process, err := backend.OpenInTmux(
		context.Background(), row, tmux.BlankLayoutName,
	)

	require.NoError(t, err)
	assert.Same(t, wantProcess, process)
}

func TestCachedLiveAttachNeverEstablishes(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	endpoint := tmux.SessionEndpoint{SessionName: "topic", SocketName: tmux.KWTServerSocketName}
	backend.resolveLive = func(context.Context, tmux.WorkspaceEndpointRequest) (tmux.SessionEndpoint, error) {
		return endpoint, nil
	}
	wantProcess := &exec.Cmd{}
	backend.prepareResidentAttach = func(context.Context, tmux.SessionEndpoint) (*exec.Cmd, error) {
		return wantProcess, nil
	}
	var ensured bool
	backend.ensureWorktree = func(context.Context, string, string, string, models.Layout) (tmux.SessionEndpoint, error) {
		ensured = true
		return tmux.SessionEndpoint{}, nil
	}
	row := dashboard.Row{
		Entry:       &discovery.GlobalWorktreeEntry{Path: "/work/topic", Branch: "topic", Generation: "0123456789abcdef0123456789abcdef"},
		SessionName: "topic", SessionLive: true, TmuxEndpoint: endpoint,
	}

	process, err := backend.OpenExistingInTmux(context.Background(), row)

	require.NoError(t, err)
	assert.Same(t, wantProcess, process)
	assert.False(t, ensured)
}

func TestTUIBackendRemovalDoesNotBlockInventoryConfiguration(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repository := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "topic")
	runTUITestGit(t, repository, "branch", "topic")
	runTUITestGit(t, repository, "worktree", "add", worktreePath, "topic")
	generation := tuiTestWorktreeGeneration(t, repository, worktreePath)
	backend := newTUIBackendWithLaunchDir(&models.Config{}, repository)
	backend.liveEndpoints = func(context.Context, tmux.WorkspaceEndpointRequest) ([]tmux.SessionEndpoint, error) { return nil, nil }
	entered := make(chan struct{})
	release := make(chan struct{})
	backend.removeWorktree = func(context.Context, kwt.RemovalRequest) (kwt.RemovalResult, error) {
		close(entered)
		<-release
		return kwt.RemovalResult{WorktreeRemoved: true}, nil
	}
	done := make(chan error, 1)
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Path: worktreePath, Branch: "topic", Generation: generation,
	}}
	go func() { done <- backend.RemoveWorktree(context.Background(), row, false) }()
	<-entered

	applied := make(chan error, 1)
	go func() { applied <- backend.applyInventoryConfig(&models.Config{}) }()
	select {
	case err := <-applied:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("inventory configuration blocked behind worktree removal")
	}

	close(release)
	require.NoError(t, <-done)
}

func TestTUIBackendRemovalPreparationDoesNotBlockInventoryConfiguration(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	entered := make(chan struct{})
	release := make(chan struct{})
	backend.resolveRemovalRoot = func(
		context.Context,
		dashboard.Row,
		*models.Config,
	) (string, error) {
		close(entered)
		<-release
		return "/repo", nil
	}
	backend.liveEndpoints = func(context.Context, tmux.WorkspaceEndpointRequest) ([]tmux.SessionEndpoint, error) {
		return nil, nil
	}
	backend.removeWorktree = func(context.Context, kwt.RemovalRequest) (kwt.RemovalResult, error) {
		return kwt.RemovalResult{WorktreeRemoved: true}, nil
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Path: "/repo/topic", Branch: "topic",
		Generation: "0123456789abcdef0123456789abcdef",
	}}
	done := make(chan error, 1)
	go func() { done <- backend.RemoveWorktree(context.Background(), row, false) }()
	<-entered

	applied := make(chan error, 1)
	go func() { applied <- backend.applyInventoryConfig(&models.Config{}) }()
	select {
	case err := <-applied:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("inventory configuration blocked behind worktree removal preparation")
	}

	close(release)
	require.NoError(t, <-done)
}

func defaultTmuxSessions(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil
	}
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil // No default server is equivalent to an empty session list.
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	sort.Strings(lines)
	return lines
}

// TestTUIBackendResolveLayoutUsesWorkspacePathForDefault covers resolveLayout's
// workspace branch: for a workspace row, the repo-local layout default must be
// read from row.Workspace.Path, not from repositoryRootForRow (which requires
// row.Entry and would error for a workspace-only row).
func TestTUIBackendResolveLayoutUsesWorkspacePathForDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KWT_HOME", filepath.Join(home, ".config", "kwt"))

	workspaceDir := t.TempDir()
	localTOML := []byte("[layouts]\ndefault = \"focus\"\n")
	localConfigPath := filepath.Join(workspaceDir, ".kwt.toml")
	require.NoError(t, os.WriteFile(localConfigPath, localTOML, 0o644))

	absPath, err := filepath.EvalSymlinks(localConfigPath)
	require.NoError(t, err)
	sum := sha256.Sum256(localTOML)
	trustStorePath := filepath.Join(home, ".config", "kwt", "trusted_configs.json")
	store, err := config.LoadTrustStore(trustStorePath)
	require.NoError(t, err)
	require.NoError(t, store.Add(absPath, hex.EncodeToString(sum[:])))

	cfg := &models.Config{
		Layouts: models.LayoutsConfig{
			Default: "quad",
			Presets: []models.Layout{
				{Name: "quad", Arrange: "tiled", Panes: []string{"shell"}},
				{Name: "focus", Arrange: "main-vertical", Panes: []string{"shell"}},
			},
		},
	}
	row := dashboard.Row{Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: workspaceDir}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	layout, err := backend.resolveLayout(row, "", false)

	require.NoError(t, err)
	assert.Equal(t, "focus", layout.Name,
		"the workspace directory's .kwt.toml default must win over the global default")
}

// The merge matches hub rows to local ones case-insensitively, so two hub rows
// differing only in identity casing would both claim the same local row and the
// last one would erase the earlier host's observation.
// Choosing the project by a case-insensitive comparison would run
// `git worktree add` inside a different repository than the one the hub row
// names, on a host where the two names are distinct repositories.
func TestTUIBackendRefusesSyncForCaseDistinctRepositoryOnUnknownHost(t *testing.T) {
	cfg := &models.Config{
		Fleet: models.FleetConfig{Enabled: true, HostID: "local-host"},
		Projects: []models.Project{{
			Repository: "git.example.com/srv/kwt",
			Name:       "kwt",
			Path:       t.TempDir(),
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	row := dashboard.Row{Fleet: &dashboard.FleetInfo{
		ProjectIdentity: "git.example.com/srv/KWT",
		Kind:            "branch",
		Ref:             "feature",
		Branch:          "feature",
		CanMaterialize:  true,
	}}

	_, err := backend.MaterializeWorktree(context.Background(), row)

	require.Error(t, err, "a differently-cased identity is a different repository here")
	assert.Contains(t, err.Error(), "no local project configured")
}

func TestTUIBackendSyncMatchesRepositoryIdentityCaseOnKnownHost(t *testing.T) {
	cfg := &models.Config{
		Projects: []models.Project{{
			Repository: "github.com/example/kwt",
			Name:       "kwt",
			Path:       t.TempDir(),
		}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	project, ok := backend.projectForFleetInfo(&dashboard.FleetInfo{
		ProjectIdentity: "github.com/Example/KWT",
	})

	require.True(t, ok, "GitHub resolves these names to one repository")
	assert.Equal(t, "github.com/example/kwt", project.Repository)
}

func TestTUIBackendMergeKeepsEveryHostForOneWorktree(t *testing.T) {
	cfg := &models.Config{Fleet: models.FleetConfig{Enabled: true, HostID: "local-host"}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	backend.now = func() time.Time { return time.Unix(1700000000, 0) }
	backend.readFleetState = func(context.Context, *models.Config) (fleet.FleetState, error) {
		// One row, as the hub now groups it, carrying both remote hosts.
		return fleet.FleetState{Rows: []fleet.FleetRow{{
			ProjectIdentity: "github.com/example/kwt",
			ProjectName:     "kwt",
			Kind:            "branch",
			Ref:             "feature",
			Branch:          "feature",
			Observations: []fleet.Observation{
				{HostID: "host-a", Path: "/w/host-a/feature", Head: "aaa"},
				{HostID: "host-b", Path: "/w/host-b/feature", Head: "bbb"},
			},
		}}}, nil
	}
	local := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		RepositoryInfo: &url.RepositoryInfo{FullPath: "github.com/example/kwt", Repository: "kwt"},
		Branch:         "feature",
		Path:           "/w/local/feature",
	}}

	rows, _ := backend.MergeFleet(context.Background(), []dashboard.Row{local})

	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Fleet)
	assert.ElementsMatch(t, []string{"host-a", "host-b", "local"}, rows[0].Fleet.Hosts,
		"every host holding this worktree must stay visible on its row")
}

func TestTUIBackendSerializesUnregisterWithFullLoad(t *testing.T) {
	cfg := &models.Config{
		Worktree:   models.WorktreeConfig{BaseDir: t.TempDir()},
		Workspaces: []models.Workspace{{Name: "notes", Path: "/Users/me/notes"}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	noEntries := func(string) ([]*discovery.GlobalWorktreeEntry, error) { return nil, nil }
	backend.discoverGlobalWorktrees = noEntries
	backend.discoverProjectWorktrees = noEntries
	backend.discoverLaunchWorktrees = noEntries
	backend.resolveSessions = resolveStoppedWorkspaceSessions
	backend.unregisterWorkspace = func(string) error { return nil }

	collecting := make(chan struct{})
	release := make(chan struct{})
	backend.collectStatuses = func(
		context.Context, string, []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, []string, error) {
		close(collecting)
		<-release
		return nil, nil, nil
	}

	var listRows []dashboard.Row
	var listErr error
	listDone := make(chan struct{})
	go func() {
		defer close(listDone)
		listRows, _, listErr = backend.List(context.Background())
	}()
	<-collecting

	unregistered := make(chan error, 1)
	go func() {
		unregistered <- backend.UnregisterWorkspace(dashboard.Row{
			Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: "/Users/me/notes"},
		})
	}()

	select {
	case <-unregistered:
		t.Fatal("unregister rewrote cfg.Workspaces while the full load was reading it")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-listDone
	require.NoError(t, listErr)
	require.NoError(t, <-unregistered)
	require.Len(t, listRows, 1, "the load that started first still sees the workspace")
	assert.Equal(t, "notes", listRows[0].Workspace.Name)
	assert.Empty(t, cfg.Workspaces)
}

func TestTUIBackendUnregisterWorkspace(t *testing.T) {
	cfg := &models.Config{Workspaces: []models.Workspace{{Name: "notes", Path: "/Users/me/notes"}}}
	backend := newTUIBackendWithLaunchDir(cfg, "")
	var removed []string
	backend.unregisterWorkspace = func(name string) error {
		removed = append(removed, name)
		return nil
	}

	err := backend.UnregisterWorkspace(dashboard.Row{
		Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: "/Users/me/notes"},
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"notes"}, removed)
	assert.Empty(t, cfg.Workspaces, "unregister must also drop the in-memory entry")

	err = backend.UnregisterWorkspace(dashboard.Row{})
	require.Error(t, err)
}
