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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/fleet"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	dashboard "go.kenn.io/kwt/internal/tui"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

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

	row := buildTUIRow(entry, status, map[string]bool{"": true})

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

	row := buildTUIRow(entry, status, map[string]bool{
		"kwt-workspace-github-com-example-kwt-feature-": false,
	})

	assert.NotEmpty(t, row.SessionName)
	assert.False(t, row.SessionLive)

	row = buildTUIRow(entry, status, map[string]bool{row.SessionName: true})
	assert.True(t, row.SessionLive)
}

func TestTUIStatusCollectorOptionsFetchesSyncState(t *testing.T) {
	opts := tuiStatusCollectorOptions("/worktrees")

	assert.True(t, opts.FetchRemote)
	assert.Equal(t, "/worktrees", opts.BaseDir)
}

func TestReadTUIFleetStatePublishesBeforeReadingHub(t *testing.T) {
	resetFleetCommandDeps(t)

	cfg := &models.Config{Fleet: models.FleetConfig{
		Enabled: true,
		HubURL:  "https://hub.example.test",
	}}
	sequence := []string{}
	client := &stubFleetClient{}
	publishFleetBestEffort = func(ctx context.Context, gotCfg *models.Config, builder fleet.ManifestBuildProvider, warn *bytes.Buffer) error {
		sequence = append(sequence, "publish")
		assert.Same(t, cfg, gotCfg)
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
	assert.Equal(t, []string{"client", "builder", "publish", "state"}, sequence,
		"the client must be validated before the expensive manifest build")
}

func TestReadTUIFleetStateIgnoresPublishWarningWithoutPanicking(t *testing.T) {
	resetFleetCommandDeps(t)

	cfg := &models.Config{Fleet: models.FleetConfig{
		Enabled: true,
		HubURL:  "https://hub.example.test",
	}}
	client := &stubFleetClient{}
	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		return &stubFleetManifestBuilder{err: errors.New("build failed")}
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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		assert.Equal(t, "/global", baseDir)
		assert.Len(t, entries, 2)
		return map[string]*models.WorktreeStatus{
			globalEntry.Path: {Path: globalEntry.Path, Branch: globalEntry.Branch},
			launchEntry.Path: {Path: launchEntry.Path, Branch: launchEntry.Branch, IsCurrent: true},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

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
	) (map[string]*models.WorktreeStatus, error) {
		t.Fatal("fast listing must not collect Git status")
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

	rows, _, err := backend.ListFast(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, models.WorktreeStatusUnknown, rows[0].Status.Status)
}

func TestTUIBackendListFastRunsIndependentDiscoveryConcurrently(t *testing.T) {
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: "/global"},
		Projects: []models.Project{{Path: "/registered"}},
	}
	backend := newTUIBackendWithLaunchDir(cfg, "/launch")
	stubTUIProjectRegistration(backend)
	started := make(chan string, 4)
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
	backend.listSessions = func() ([]string, error) {
		block("sessions")
		return nil, nil
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := backend.ListFast(context.Background())
		done <- err
	}()

	for range 4 {
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
	) (map[string]*models.WorktreeStatus, error) {
		assert.ElementsMatch(t, []string{globalEntry.Path, projectEntry.Path}, []string{
			entries[0].Path,
			entries[1].Path,
		})
		return map[string]*models.WorktreeStatus{
			globalEntry.Path:  {Path: globalEntry.Path, Branch: globalEntry.Branch},
			projectEntry.Path: {Path: projectEntry.Path, Branch: projectEntry.Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

	rows, _, err := backend.List(context.Background())

	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{globalEntry.Path, projectEntry.Path}, []string{
		rowPathForHandoff(rows[0]),
		rowPathForHandoff(rows[1]),
	})
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
	) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{
			localEntry.Path: {Path: localEntry.Path, Branch: localEntry.Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{
			localEntry.Path: {Path: localEntry.Path, Branch: localEntry.Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{
			localEntry.Path: {Path: localEntry.Path, Branch: localEntry.Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{
			detachedEntry.Path: {Path: detachedEntry.Path},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		require.Len(t, entries, 1)
		return map[string]*models.WorktreeStatus{
			entries[0].Path: {Path: entries[0].Path, Branch: entries[0].Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

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
	) (map[string]*models.WorktreeStatus, error) {
		require.Len(t, entries, 1)
		require.NotNil(t, entries[0].RepositoryInfo)
		assert.Equal(t, "local.example/team/service", entries[0].RepositoryInfo.FullPath)
		return map[string]*models.WorktreeStatus{
			entries[0].Path: {Path: entries[0].Path, Branch: entries[0].Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

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
	backend.registerProject = func(project models.Project) error {
		registered = append(registered, project)
		return errors.New("read-only config")
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{
			launchEntry.Path: {Path: launchEntry.Path, Branch: launchEntry.Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

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
	backend.registerProject = func(models.Project) error {
		registrations++
		return nil
	}
	backend.collectStatuses = func(
		context.Context,
		string,
		[]*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

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
	backend.registerProject = func(project models.Project) error {
		return nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{
			launchEntry.Path: {Path: launchEntry.Path, Branch: launchEntry.Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

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
	backend.registerProject = func(project models.Project) error {
		registered = append(registered, project)
		return nil
	}
	backend.collectStatuses = func(
		ctx context.Context,
		baseDir string,
		entries []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, error) {
		return map[string]*models.WorktreeStatus{
			launchEntry.Path: {Path: launchEntry.Path, Branch: launchEntry.Branch},
		}, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }

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
	backend.registerProject = func(project models.Project) error {
		registered = append(registered, project)
		return nil
	}

	backend.registerLaunchProject([]*discovery.GlobalWorktreeEntry{launchEntry})

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
	backend.registerProject = func(project models.Project) error {
		registered = append(registered, project)
		return nil
	}

	backend.registerLaunchProject([]*discovery.GlobalWorktreeEntry{launchEntry})

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
	backend.registerProject = func(project models.Project) error {
		registered = append(registered, project)
		return nil
	}

	backend.registerLaunchProject([]*discovery.GlobalWorktreeEntry{launchEntry})

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
	backend.registerProject = func(project models.Project) error {
		registered = append(registered, project)
		return nil
	}

	backend.registerLaunchProject([]*discovery.GlobalWorktreeEntry{launchEntry})

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
	backend.registerProject = func(models.Project) error { return nil }

	backend.registerLaunchProject(launchEntries)
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "stale-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/stale", worktreePath)
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
		Branch: "codex/stale",
		Path:   worktreePath,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	output := runTUITestGitOutput(t, repoPath, "worktree", "list", "--porcelain")
	assert.NotContains(t, output, worktreePath)
}

func TestTUIBackendCreateWorktreePublishesAfterSuccessfulMutation(t *testing.T) {
	resetFleetCommandDeps(t)
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
		assert.Same(t, cfg, gotCfg)
		assert.NotNil(t, builder)
		assert.NotNil(t, warn)
		return errors.New("hub unavailable")
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch: "main",
		Path:   repoPath,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	path, err := backend.CreateWorktree(context.Background(), row, "feature/from-tui")

	require.NoError(t, err)
	assert.DirExists(t, path)
	assert.Equal(t, 1, published)
}

func TestTUIBackendCreateWorktreeDoesNotExpandRepositoryLocalTemplate(t *testing.T) {
	const secret = "credential-must-not-appear-in-path"
	t.Setenv("KWT_GITHUB_TOKEN", secret)

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	cfg := &models.Config{
		Worktree: models.WorktreeConfig{BaseDir: filepath.Join(t.TempDir(), "worktrees"), AutoMkdir: true},
		Naming: models.NamingConfig{
			Template:        `{{printf "%c%s" 36 "KWT_GITHUB_TOKEN"}}/{{.Branch}}`,
			RepositoryLocal: true,
		},
	}
	row := dashboard.Row{Entry: &discovery.GlobalWorktreeEntry{
		Branch: "main",
		Path:   repoPath,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	planned, err := backend.PreviewWorktree(row, "feature/from-tui")
	require.NoError(t, err)
	path, err := backend.CreateWorktree(context.Background(), row, "feature/from-tui")

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
		assert.Same(t, cfg, gotCfg)
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
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	assert.Equal(t, 1, published)
}

func TestTUIBackendRemoveWorktreeDirtyErrorDoesNotSuggestCLIForce(t *testing.T) {
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
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "uncommitted changes")
	assert.NotContains(t, err.Error(), "kwt remove --force")
	assert.DirExists(t, worktreePath)
}

func TestTUIBackendForceRemoveDeletesDirtyWorktree(t *testing.T) {
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
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	err := backend.RemoveWorktree(context.Background(), row, true)

	require.NoError(t, err)
	assert.NoDirExists(t, worktreePath)
}

func TestTUIBackendMaterializeWorktreeUsesRegisteredProjectRoot(t *testing.T) {
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
}

func TestTUIBackendMaterializeWorktreeDeletesAutoCreatedBranchOnStaleHead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "remote", "add", "origin", "https://github.com/example/kwt.git")
	// Simulate a fetched remote branch with no local counterpart, so
	// `git worktree add` auto-creates the local branch.
	runTUITestGit(t, repoPath, "update-ref", "refs/remotes/origin/feature/studio-only", "HEAD")
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
}

func tuiTestBranchExists(repoPath string, branch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoPath
	return cmd.Run() == nil
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

func TestTUIBackendRemoveWorktreeRepairsBrokenGitFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "broken-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/broken", worktreePath)
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
		Branch: "codex/broken",
		Path:   worktreePath,
	}}
	backend := newTUIBackendWithLaunchDir(cfg, "")

	err := backend.RemoveWorktree(context.Background(), row, false)

	require.NoError(t, err)
	output := runTUITestGitOutput(t, repoPath, "worktree", "list", "--porcelain")
	assert.NotContains(t, output, worktreePath)
	assert.NoDirExists(t, worktreePath)
}

func TestRepairLinkedWorktreeGitFileRejectsSymlink(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "symlink-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/symlink", worktreePath)

	victimPath := filepath.Join(t.TempDir(), "victim")
	const victimContents = "do not replace me\n"
	require.NoError(t, os.WriteFile(victimPath, []byte(victimContents), 0644))

	gitFilePath := filepath.Join(worktreePath, ".git")
	require.NoError(t, os.Remove(gitFilePath))
	if err := os.Symlink(victimPath, gitFilePath); err != nil {
		t.Skipf("symbolic links are not supported or allowed on this filesystem: %v", err)
	}

	err := repairLinkedWorktreeGitFile(repoPath, worktreePath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "symbolic link")
	data, readErr := os.ReadFile(victimPath)
	require.NoError(t, readErr)
	assert.Equal(t, victimContents, string(data))
}

func TestRepairLinkedWorktreeGitFileReplacesHardLinkWithoutClobberingTarget(t *testing.T) {
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "hardlink-worktree")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "codex/hardlink", worktreePath)

	victimPath := filepath.Join(t.TempDir(), "victim")
	const victimContents = "do not replace me\n"
	require.NoError(t, os.WriteFile(victimPath, []byte(victimContents), 0644))

	gitFilePath := filepath.Join(worktreePath, ".git")
	require.NoError(t, os.Remove(gitFilePath))
	if err := os.Link(victimPath, gitFilePath); err != nil {
		t.Skipf("hard links are not supported on this filesystem: %v", err)
	}

	err := repairLinkedWorktreeGitFile(repoPath, worktreePath)

	require.NoError(t, err)
	victimData, readErr := os.ReadFile(victimPath)
	require.NoError(t, readErr)
	assert.Equal(t, victimContents, string(victimData))
	gitData, readErr := os.ReadFile(gitFilePath)
	require.NoError(t, readErr)
	assert.Contains(t, string(gitData), "gitdir: ")
	assert.NotEqual(t, string(gitData), string(victimData))
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

func stubTUIProjectRegistration(backend *tuiBackend) {
	backend.registerProject = func(models.Project) error { return nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	liveSession := tmux.DirWorkspaceSessionName("old-name", dir)
	backend.listSessions = func() ([]string, error) { return []string{liveSession}, nil }

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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	) (map[string]*models.WorktreeStatus, error) {
		return nil, nil
	}
	backend.listSessions = func() ([]string, error) { return nil, nil }
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
	assert.Equal(t, "live-session-name", name,
		"a non-empty SessionName must win over the workspace branch")
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

func TestTUIBackendAttachWorkspaceGuardRejectsEmptyRow(t *testing.T) {
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")

	err := backend.attachWorkspace(context.Background(), dashboard.Row{}, "", false, false)

	require.Error(t, err)
	assert.Equal(t, "no worktree selected", err.Error())
}

func TestTUIBackendRefusesProtectedPullRequestWorkspace(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	workspacePath := t.TempDir()
	require.NoError(t, pullrequest.NewFileStore(prStorePath()).Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["pr-32"] = pullrequest.Provenance{
				Workspace: pullrequest.Workspace{Path: workspacePath},
			}
			return nil
		},
	))
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	called := false
	backend.ensureAndAttach = func(
		context.Context,
		string,
		string,
		models.Layout,
		bool,
	) error {
		called = true
		return nil
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
	row := dashboard.Row{
		Workspace: &dashboard.WorkspaceInfo{Name: "notes", Path: t.TempDir()},
	}
	backend := newTUIBackendWithLaunchDir(&models.Config{}, "")
	before := defaultTmuxSessions(t)

	var gotSession, gotRoot string
	var gotLayout models.Layout
	var gotInsideTmux bool
	backend.ensureAndAttach = func(
		ctx context.Context,
		session, root string,
		layout models.Layout,
		insideTmux bool,
	) error {
		gotSession, gotRoot = session, root
		gotLayout, gotInsideTmux = layout, insideTmux
		return nil
	}

	err := backend.attachWorkspace(context.Background(), row, tmux.BlankLayoutName, false, false)

	require.NoError(t, err)
	assert.Equal(t, tmux.DirWorkspaceSessionName("notes", row.Workspace.Path), gotSession)
	assert.Equal(t, row.Workspace.Path, gotRoot)
	assert.Equal(t, tmux.BlankLayout(), gotLayout)
	assert.False(t, gotInsideTmux)
	assert.Equal(t, before, defaultTmuxSessions(t),
		"the stubbed command-layer test must not add default-server tmux sessions")
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
	backend.listSessions = func() ([]string, error) { return nil, nil }
	backend.unregisterWorkspace = func(string) error { return nil }

	collecting := make(chan struct{})
	release := make(chan struct{})
	backend.collectStatuses = func(
		context.Context, string, []*discovery.GlobalWorktreeEntry,
	) (map[string]*models.WorktreeStatus, error) {
		close(collecting)
		<-release
		return nil, nil
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
