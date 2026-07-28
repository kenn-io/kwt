package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/fleet"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/pkg/models"
)

func TestAddPublishesBestEffortAfterSuccessfulCreation(t *testing.T) {
	resetFleetCommandDeps(t)
	resetAddCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	addBranch = true
	addNoLaunch = true
	worktreePath := filepath.Join(t.TempDir(), "task7-add-publish")
	sequence := []string{}

	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		sequence = append(sequence, "builder")
		return &stubFleetManifestBuilder{}
	}
	publishFleetBestEffort = func(ctx context.Context, cfg *models.Config, builder fleet.ManifestBuildProvider, warn *bytes.Buffer) error {
		sequence = append(sequence, "publish")
		assert.NotNil(t, ctx)
		assert.True(t, cfg.Fleet.Enabled)
		assert.NotNil(t, builder)
		assert.DirExists(t, worktreePath, "publish should run after the worktree exists")
		return errors.New("publish failed")
	}

	cmd, _, stderr := fleetTestCommand()
	err := runAdd(cmd, []string{"task7/add-publish", worktreePath})

	require.NoError(t, err)
	assert.Equal(t, []string{"builder", "publish"}, sequence)
	assert.Contains(t, stderr.String(), "warning: sync publish failed: publish failed")
}

func TestAddDoesNotPublishWhenValidationFails(t *testing.T) {
	resetFleetCommandDeps(t)
	resetAddCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	addBranch = true
	addNoLaunch = true
	occupiedPath := filepath.Join(t.TempDir(), "occupied")
	require.NoError(t, os.MkdirAll(occupiedPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(occupiedPath, "file.txt"), []byte("busy"), 0644))

	var calls int
	publishFleetBestEffortForCommand = func(*cobra.Command, *models.Config) {
		calls++
	}

	cmd, _, _ := fleetTestCommand()
	err := runAdd(cmd, []string{"task7/add-fails", occupiedPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory is not empty")
	assert.Zero(t, calls)
}

func TestAddFromRemoteBranchCreatesTrackingWorktree(t *testing.T) {
	resetFleetCommandDeps(t)
	resetAddCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	remotePath := filepath.Join(t.TempDir(), "origin.git")
	runTUITestGit(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
	runTUITestGit(t, repoPath, "remote", "add", "origin", remotePath)
	runTUITestGit(t, repoPath, "checkout", "-b", "feature/remote")
	runTUITestGit(t, repoPath, "push", "origin", "feature/remote")
	runTUITestGit(t, repoPath, "checkout", "main")
	runTUITestGit(t, repoPath, "branch", "-D", "feature/remote")

	addFrom = "origin/feature/remote"
	addNoLaunch = true
	worktreePath := filepath.Join(t.TempDir(), "feature-remote")

	cmd, _, _ := fleetTestCommand()
	err := runAdd(cmd, []string{"feature/remote", worktreePath})

	require.NoError(t, err)
	assert.Equal(
		t,
		"origin/feature/remote",
		strings.TrimSpace(runTUITestGitOutput(
			t,
			worktreePath,
			"rev-parse",
			"--abbrev-ref",
			"@{upstream}",
		)),
	)
}

func TestShouldLaunch(t *testing.T) {
	cases := []struct {
		name             string
		autoLaunch       bool
		layoutFlagPassed bool
		noLaunch         bool
		want             bool
		wantErr          bool
	}{
		{"default on", true, false, false, true, false},
		{"default off", false, false, false, false, false},
		{"flag forces launch when default off", false, true, false, true, false},
		{"no-launch suppresses", true, false, true, false, false},
		{"no-launch plus flag errors", false, true, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := shouldLaunch(tc.autoLaunch, tc.layoutFlagPassed, tc.noLaunch)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPrepareLaunchUsesLocalRepositoryFallback(t *testing.T) {
	putFakeTmuxOnPath(t)
	oldLayout, oldSelectLayout := addLayout, addSelectLayout
	t.Cleanup(func() {
		addLayout = oldLayout
		addSelectLayout = oldSelectLayout
	})
	addLayout = ""
	addSelectLayout = false

	repoPath := newTUITestRepo(t)
	ctx := &CommandContext{
		Config: &models.Config{
			Layouts: models.LayoutsConfig{
				Default: "shell",
				Presets: []models.Layout{{
					Name:    "shell",
					Arrange: "tiled",
					Panes:   []string{""},
				}},
			},
		},
		Git: git.New(repoPath),
	}

	layout, info, err := prepareLaunch(ctx)

	require.NoError(t, err)
	assert.Equal(t, "shell", layout.Name)
	require.NotNil(t, info)
	assert.Equal(t, filepath.Base(repoPath), info.Repository)
	assert.True(t, strings.HasPrefix(info.FullPath, "local/"), info.FullPath)
}

func putFakeTmuxOnPath(t *testing.T) {
	t.Helper()

	name := "tmux"
	if runtime.GOOS == "windows" {
		name = "tmux.exe"
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(""), 0755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func resetAddCommandFlags(t *testing.T) {
	t.Helper()

	oldAddBranch := addBranch
	oldAddInteractive := addInteractive
	oldAddForce := addForce
	oldAddExpires := addExpires
	oldAddLayout := addLayout
	oldAddSelectLayout := addSelectLayout
	oldAddNoLaunch := addNoLaunch
	oldAddFrom := addFrom

	t.Cleanup(func() {
		addBranch = oldAddBranch
		addInteractive = oldAddInteractive
		addForce = oldAddForce
		addExpires = oldAddExpires
		addLayout = oldAddLayout
		addSelectLayout = oldAddSelectLayout
		addNoLaunch = oldAddNoLaunch
		addFrom = oldAddFrom
	})

	addBranch = false
	addInteractive = false
	addForce = false
	addExpires = ""
	addLayout = ""
	addSelectLayout = false
	addNoLaunch = false
	addFrom = ""
}

func initCommandTestConfig(t *testing.T, baseDir string) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)

	home := t.TempDir()
	kwtHome := filepath.Join(home, "kwt")
	require.NoError(t, os.MkdirAll(kwtHome, 0755))
	t.Setenv("HOME", home)
	t.Setenv("KWT_HOME", kwtHome)
	t.Setenv("KWT_FLEET_TOKEN", "secret")

	configText := fmt.Sprintf(`[worktree]
basedir = %q
auto_mkdir = true

[fleet]
enabled = true
host_id = "test-host"
hub_url = "https://hub.example.test"
token_env = "KWT_FLEET_TOKEN"

[layouts]
default = "shell"
auto_launch_on_add = false

[[layouts.presets]]
name = "shell"
arrange = "tiled"
panes = [""]
`, baseDir)
	require.NoError(t, os.WriteFile(filepath.Join(kwtHome, "config.toml"), []byte(configText), 0600))
	require.NoError(t, config.Init())
}
