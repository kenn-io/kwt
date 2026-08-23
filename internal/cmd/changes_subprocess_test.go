package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/service"
)

func TestChangesSubprocessInspectsPrimaryAndLinkedWorktreesWithGuards(t *testing.T) {
	binary, cleanupBinary := buildDaemonTestBinaries(t)
	primary, linked := newChangesSubprocessWorktrees(t)
	home := newChangesSubprocessHome(t, primary)
	registerDaemonCleanup(t, cleanupBinary, home)

	primaryResult := runChangesSubprocessJSON(t, binary, home, primary, primary)
	linkedResult := runChangesSubprocessJSON(t, binary, home, linked, linked)
	assert.Equal(t, "github.com/acme/widget", primaryResult.Worktree.Repository)
	assert.Equal(t, "github.com/acme/widget", linkedResult.Worktree.Repository)
	assert.Equal(
		t,
		utils.PathKey(canonicalTestPath(t, primary)),
		utils.PathKey(primaryResult.Worktree.Path),
	)
	assert.Equal(
		t,
		utils.PathKey(canonicalTestPath(t, linked)),
		utils.PathKey(linkedResult.Worktree.Path),
	)
	assert.NotEqual(t, primaryResult.Worktree.Generation, linkedResult.Worktree.Generation)

	stdout, stderr, err := runInventoryCommand(
		t,
		binary,
		home,
		linked,
		"changes",
		linked,
		"--expected-repository",
		linkedResult.Worktree.Repository,
		"--expected-generation",
		linkedResult.Worktree.Generation,
		"--json",
	)
	require.NoError(t, err, "stdout=%s stderr=%s", stdout, stderr)
	var guarded kwt.InspectionResult
	require.NoError(t, json.Unmarshal(stdout, &guarded))
	assert.Equal(t, linkedResult.Worktree, guarded.Worktree)

	for _, mismatch := range []struct {
		name string
		args []string
	}{
		{
			name: "repository",
			args: []string{
				"--expected-repository", "github.com/other/widget",
			},
		},
		{
			name: "generation",
			args: []string{
				"--expected-generation", primaryResult.Worktree.Generation,
			},
		},
	} {
		t.Run(mismatch.name+" mismatch", func(t *testing.T) {
			args := append([]string{"changes", linked}, mismatch.args...)
			args = append(args, "--json")
			stdout, stderr, err := runInventoryCommand(
				t, binary, home, linked, args...,
			)
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "stdout=%s stderr=%s", stdout, stderr)
			assert.Equal(t, 1, exitErr.ExitCode())
			var envelope jsonErrorEnvelope
			require.NoError(t, json.Unmarshal(stdout, &envelope))
			assert.Equal(t, service.RegistrationChanged, envelope.Error.Code)
			assert.True(t, envelope.Error.Retryable)
		})
	}
}

func TestChangesSubprocessPreservesUnusualFilenamesAndCommandBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command recorder uses a POSIX shell")
	}
	binary, cleanupBinary := buildDaemonTestBinaries(t)
	primary, _ := newChangesSubprocessWorktrees(t)
	home := newChangesSubprocessHome(t, primary)
	registerDaemonCleanup(t, cleanupBinary, home)
	filenames := []string{
		"line\nbreak.txt",
		"space name.txt",
		"tab\tname.txt",
	}
	for _, name := range filenames {
		require.NoError(t, os.WriteFile(
			filepath.Join(primary, name),
			[]byte("untracked\n"),
			0o644,
		))
	}

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	wrapperDir := t.TempDir()
	logPath := filepath.Join(wrapperDir, "git.log")
	wrapperPath := filepath.Join(wrapperDir, "git")
	require.NoError(t, os.WriteFile(wrapperPath, []byte(`#!/bin/sh
{
  printf 'git'
  for argument in "$@"; do
    printf '\t%s' "$argument"
  done
  printf '\n'
} >> "$KWT_TEST_GIT_LOG"
exec "$KWT_TEST_REAL_GIT" "$@"
`), 0o755))
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KWT_TEST_GIT_LOG", logPath)
	t.Setenv("KWT_TEST_REAL_GIT", realGit)

	result := runChangesSubprocessJSON(t, binary, home, primary, primary)
	paths := make([]string, len(result.Changes.Files))
	for index, file := range result.Changes.Files {
		paths[index] = file.Path
	}
	slices.Sort(filenames)
	assert.Equal(t, filenames, paths)
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	commands := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	var statusReads int
	for _, command := range commands {
		fields := strings.Split(command, "\t")
		if slices.Equal(fields, []string{
			"git", "status", "--porcelain=v2", "-z", "--untracked-files=all",
		}) {
			statusReads++
		}
		for _, forbidden := range []string{"fetch", "diff", "numstat"} {
			assert.NotContains(t, fields[1:], forbidden, "command=%q", command)
		}
	}
	assert.Equal(t, 1, statusReads, "commands=%q", commands)

	stdout, stderr, err := runInventoryCommand(
		t, binary, home, primary, "status", "--json", "--no-fetch",
	)
	require.NoError(t, err, "stdout=%s stderr=%s", stdout, stderr)
	var legacy map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(stdout, &legacy))
	assert.ElementsMatch(t, []string{"summary", "worktrees"}, mapKeys(legacy))
	assert.NotContains(t, string(stdout), `"files"`)
	assert.NotContains(t, string(stdout), `"worktree":{"repository"`)
}

func TestChangesSubprocessRejectsInventoryWithoutConfigCapability(t *testing.T) {
	binary, _ := buildDaemonTestBinaries(t)
	fixture := buildDaemonFixture(t)
	primary, _ := newChangesSubprocessWorktrees(t)
	home := newDaemonTestHome(t, strings.Replace(
		validDaemonConfig+fmt.Sprintf(`
[fleet]
token_env = "CUSTOM_FLEET_TOKEN"

[[projects]]
repository = "github.com/acme/widget"
name = "widget"
path = %q
`, filepath.ToSlash(primary)),
		`auto_restart = "newer"`,
		`auto_restart = "never"`,
		1,
	))
	t.Setenv("CUSTOM_FLEET_TOKEN", "must-not-reach-git")
	startDaemonFixture(t, fixture, home, "legacy_inventory")

	stdout, stderr, err := runInventoryCommand(
		t,
		binary,
		home,
		primary,
		"changes",
		primary,
		"--json",
	)

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "stdout=%s stderr=%s", stdout, stderr)
	assert.Equal(t, 1, exitErr.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout, &envelope))
	assert.Equal(t, service.DaemonIncompatible, envelope.Error.Code)
	assert.False(t, envelope.Error.Retryable)
}

func runChangesSubprocessJSON(
	t *testing.T,
	binary string,
	home string,
	directory string,
	path string,
) kwt.InspectionResult {
	t.Helper()
	stdout, stderr, err := runInventoryCommand(
		t, binary, home, directory, "changes", path, "--json",
	)
	require.NoError(t, err, "stdout=%s stderr=%s", stdout, stderr)
	var result kwt.InspectionResult
	require.NoError(t, json.Unmarshal(stdout, &result), "stdout=%s", stdout)
	return result
}

func newChangesSubprocessWorktrees(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	linked := filepath.Join(root, "linked")
	runTUITestGit(t, "", "init", "-b", "main", primary)
	runTUITestGit(t, primary, "config", "user.name", "Test User")
	runTUITestGit(t, primary, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(
		filepath.Join(primary, "README.md"),
		[]byte("# widget\n"),
		0o644,
	))
	runTUITestGit(t, primary, "add", ".")
	runTUITestGit(t, primary, "commit", "-m", "Initial commit")
	runTUITestGit(t, primary, "worktree", "add", "-b", "feature", linked)
	worktrees, err := git.New(primary).ListWorktrees()
	require.NoError(t, err)
	require.Len(t, worktrees, 2)
	return canonicalTestPath(t, primary), canonicalTestPath(t, linked)
}

func newChangesSubprocessHome(t *testing.T, primary string) string {
	t.Helper()
	return newDaemonTestHome(t, validDaemonConfig+fmt.Sprintf(`
[[projects]]
repository = "github.com/acme/widget"
name = "widget"
path = %q
`, filepath.ToSlash(primary)))
}

func canonicalTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return resolved
}

func mapKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
