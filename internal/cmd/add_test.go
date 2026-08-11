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
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/fleet"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func TestRegisteredAddLosesToProjectRemoval(t *testing.T) {
	resetFleetCommandDeps(t)
	resetAddCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)
	configPath := filepath.Join(os.Getenv("KWT_HOME"), "config.toml")
	file, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = fmt.Fprintf(
		file,
		"\n[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'widget'\npath = %q\n",
		repoPath,
	)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	oldBeforeAcquire := beforeProjectGuardAcquire
	t.Cleanup(func() { beforeProjectGuardAcquire = oldBeforeAcquire })
	beforeProjectGuardAcquire = func() {
		snapshot, snapshotErr := config.LoadGlobalSnapshotAt(os.Getenv("KWT_HOME"))
		require.NoError(t, snapshotErr)
		changed, removeErr := config.CompareAndSwapProjectAt(
			os.Getenv("KWT_HOME"), snapshot.Projects[0], nil,
		)
		require.NoError(t, removeErr)
		require.True(t, changed)
	}
	addBranch = true
	addNoLaunch = true
	worktreePath := filepath.Join(t.TempDir(), "feature-guarded")
	cmd, _, _ := fleetTestCommand()

	err = runAdd(cmd, []string{"feature/guarded", worktreePath})

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	assert.NoDirExists(t, worktreePath)
}

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

func TestAddFromRemoteBranchCreatesTrackingWorktreeWithoutLaunching(t *testing.T) {
	resetFleetCommandDeps(t)
	resetAddCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	viper.Set("layouts.auto_launch_on_add", true)
	putFakeTmuxOnPath(t)
	t.Chdir(repoPath)

	remotePath := filepath.Join(t.TempDir(), "origin.git")
	runTUITestGit(t, filepath.Dir(remotePath), "init", "--bare", "-b", "main", remotePath)
	runTUITestGit(t, repoPath, "remote", "add", "origin", remotePath)
	runTUITestGit(t, repoPath, "checkout", "-b", "feature/remote")
	runTUITestGit(t, repoPath, "push", "origin", "feature/remote")
	runTUITestGit(t, repoPath, "checkout", "main")
	runTUITestGit(t, repoPath, "branch", "-D", "feature/remote")

	addFrom = "origin/feature/remote"
	addExpires = "1h"
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
	reg, registryErr := registry.New()
	require.NoError(t, registryErr)
	entry, ok := reg.Get(worktreePath)
	require.True(t, ok)
	assert.True(t, entry.UnreviewedRemoteSource)
	assert.NotNil(t, entry.ExpiresAt)
	assert.NotEmpty(t, entry.Generation)
}

func TestRegisterWorktreeExpirationRejectsRecreatedWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "reused-worktree")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"feature/original",
		worktreePath,
	)
	originalGeneration := tuiTestWorktreeGeneration(
		t,
		repoPath,
		worktreePath,
	)
	runTUITestGit(t, repoPath, "worktree", "remove", "--force", worktreePath)
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"feature/replacement",
		worktreePath,
	)
	replacementGeneration := tuiTestWorktreeGeneration(
		t,
		repoPath,
		worktreePath,
	)

	reg, err := registry.New()
	require.NoError(t, err)
	replacementExpiry := time.Now().Add(2 * time.Hour)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:       worktreePath,
		Branch:     "feature/replacement",
		Generation: replacementGeneration,
		ExpiresAt:  &replacementExpiry,
	}))
	staleExpiry := time.Now().Add(time.Hour)

	err = registerWorktreeExpiration(
		git.New(repoPath),
		reg,
		worktreePath,
		originalGeneration,
		"feature/original",
		&staleExpiry,
	)

	require.Error(t, err)
	entry, ok := reg.Get(worktreePath)
	require.True(t, ok)
	assert.Equal(t, "feature/replacement", entry.Branch)
	assert.Equal(t, replacementGeneration, entry.Generation)
	require.NotNil(t, entry.ExpiresAt)
	assert.True(t, entry.ExpiresAt.Equal(replacementExpiry))
}

func TestRegisterWorktreeExpirationCreatesOrdinaryWorktreeEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "ordinary-worktree")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"feature/ordinary",
		worktreePath,
	)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	reg, err := registry.New()
	require.NoError(t, err)
	expiresAt := time.Now().Add(time.Hour)

	err = registerWorktreeExpiration(
		git.New(repoPath),
		reg,
		worktreePath,
		generation,
		"feature/ordinary",
		&expiresAt,
	)

	require.NoError(t, err)
	entry, ok := reg.Get(worktreePath)
	require.True(t, ok)
	assert.Equal(t, generation, entry.Generation)
	assert.Equal(t, "feature/ordinary", entry.Branch)
	require.NotNil(t, entry.ExpiresAt)
	assert.True(t, entry.ExpiresAt.Equal(expiresAt))
}

func TestRegisterWorktreeExpirationUsesDestinationRemoteIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "destination-origin")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "feature/destination-origin", worktreePath)
	runTUITestGit(t, repoPath, "config", "extensions.worktreeConfig", "true")
	runTUITestGit(t, worktreePath, "config", "--worktree", "remote.origin.url", "https://github.com/acme/destination.git")
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	reg, err := registry.New()
	require.NoError(t, err)
	expiresAt := time.Now().Add(time.Hour)

	err = registerWorktreeExpiration(
		git.New(repoPath), reg, worktreePath, generation,
		"feature/destination-origin", &expiresAt,
	)

	require.NoError(t, err)
	entry, ok := reg.Get(worktreePath)
	require.True(t, ok)
	assert.Equal(t, "github.com/acme/destination", entry.Repository)
}

func TestRegisterWorktreeExpirationRejectsRelativeDestinationRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "relative-origin")
	runTUITestGit(t, repoPath, "worktree", "add", "-b", "feature/relative-origin", worktreePath)
	runTUITestGit(t, repoPath, "config", "extensions.worktreeConfig", "true")
	runTUITestGit(t, worktreePath, "config", "--worktree", "remote.origin.url", "credentials/team/repo.git")
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	reg, err := registry.New()
	require.NoError(t, err)
	expiresAt := time.Now().Add(time.Hour)

	err = registerWorktreeExpiration(
		git.New(repoPath), reg, worktreePath, generation,
		"feature/relative-origin", &expiresAt,
	)

	require.NoError(t, err)
	entry, ok := reg.Get(worktreePath)
	require.True(t, ok)
	assert.Empty(t, entry.Repository)
}

func TestRegisterWorktreeExpirationRejectsProvisionalCreation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := newTUITestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "creating-worktree")
	runTUITestGit(
		t,
		repoPath,
		"worktree",
		"add",
		"-b",
		"feature/creating",
		worktreePath,
	)
	generation := tuiTestWorktreeGeneration(t, repoPath, worktreePath)
	reg, err := registry.New()
	require.NoError(t, err)
	require.NoError(t, reg.Register(&registry.WorktreeEntry{
		Path:          worktreePath,
		Branch:        "feature/creating",
		CreationToken: "active-creation",
	}))
	expiresAt := time.Now().Add(time.Hour)

	err = registerWorktreeExpiration(
		git.New(repoPath),
		reg,
		worktreePath,
		generation,
		"feature/creating",
		&expiresAt,
	)

	require.ErrorContains(t, err, "worktree creation in progress")
	entry, ok := reg.Get(worktreePath)
	require.True(t, ok)
	assert.Equal(t, "active-creation", entry.CreationToken)
	assert.Empty(t, entry.Generation)
	assert.Nil(t, entry.ExpiresAt)
}

func TestAddFromResolvesRemoteRefAcrossLocalNamespaceCollision(t *testing.T) {
	resetFleetCommandDeps(t)
	resetAddCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)
	runTUITestGit(t, repoPath, "remote", "add", "origin", repoPath)
	runTUITestGit(
		t,
		repoPath,
		"update-ref",
		"refs/remotes/origin/topic",
		"HEAD",
	)
	runTUITestGit(t, repoPath, "branch", "origin/topic")

	addFrom = "origin/topic"
	worktreePath := filepath.Join(t.TempDir(), "imported-topic")
	cmd, _, _ := fleetTestCommand()

	err := runAdd(cmd, []string{"imported-topic", worktreePath})

	require.NoError(t, err)
	assert.Equal(
		t,
		"origin",
		strings.TrimSpace(runTUITestGitOutput(
			t,
			repoPath,
			"config",
			"branch.imported-topic.remote",
		)),
	)
	assert.Equal(
		t,
		"refs/heads/topic",
		strings.TrimSpace(runTUITestGitOutput(
			t,
			repoPath,
			"config",
			"branch.imported-topic.merge",
		)),
	)
}

func TestAddExistingLocalBranchDefersWorkspaceLaunchUntilReview(t *testing.T) {
	resetFleetCommandDeps(t)
	resetAddCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	viper.Set("layouts.auto_launch_on_add", true)
	putFakeTmuxOnPath(t)
	runTUITestGit(t, repoPath, "checkout", "-b", "feature/local")
	runTUITestGit(t, repoPath, "checkout", "main")
	t.Chdir(repoPath)

	runner := &recordingOpenWorkspaceRunner{}
	oldNewRunner := newAddWorkspaceRunner
	t.Cleanup(func() { newAddWorkspaceRunner = oldNewRunner })
	newAddWorkspaceRunner = func([]string) openWorkspaceRunner {
		return runner
	}
	worktreePath := filepath.Join(t.TempDir(), "feature-local")

	cmd, _, _ := fleetTestCommand()
	err := runAdd(cmd, []string{"feature/local", worktreePath})

	require.NoError(t, err)
	assert.False(t, runner.attached)
	reg, err := registry.New()
	require.NoError(t, err)
	assert.True(t, reg.IsUnreviewedRemoteSource(worktreePath))
}

func TestExistingBranchReviewMessageUsesStaticPicker(t *testing.T) {
	assert.Equal(
		t,
		"Review the existing-branch checkout, then run 'kwt open' to select it and start its workspace.",
		existingBranchReviewMessage(),
	)
}

func TestAddFromRemoteBranchRejectsImmediateLayoutLaunch(t *testing.T) {
	resetFleetCommandDeps(t)
	resetAddCommandFlags(t)

	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)

	addFrom = "origin/feature/remote"
	addLayout = "shell"
	worktreePath := filepath.Join(t.TempDir(), "feature-remote")

	cmd, _, _ := fleetTestCommand()
	err := runAdd(cmd, []string{"feature/remote", worktreePath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "inspect the checkout")
	assert.NoDirExists(t, worktreePath)
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

func TestAttachWorkspacePassesConfiguredCredentialName(t *testing.T) {
	runner := &recordingOpenWorkspaceRunner{}
	var protectedNames []string
	oldNewRunner := newAddWorkspaceRunner
	t.Cleanup(func() { newAddWorkspaceRunner = oldNewRunner })
	newAddWorkspaceRunner = func(names []string) openWorkspaceRunner {
		protectedNames = append([]string(nil), names...)
		return runner
	}
	cfg := &models.Config{
		Fleet: models.FleetConfig{TokenEnv: "Custom_Fleet_Token"},
	}

	err := attachWorkspace(
		&url.RepositoryInfo{FullPath: "github.com/acme/widget"},
		"feature",
		t.TempDir(),
		tmux.BlankLayout(),
		cfg,
	)

	require.NoError(t, err)
	assert.True(t, runner.attached)
	assert.ElementsMatch(
		t,
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN", "Custom_Fleet_Token"},
		protectedNames,
	)
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
