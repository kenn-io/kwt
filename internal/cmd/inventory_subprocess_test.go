package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func runInventoryCommand(
	t *testing.T,
	binary, home, directory string,
	args ...string,
) ([]byte, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "KWT_HOME="+home)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func buildDaemonFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	path := filepath.Join(t.TempDir(), "daemonfixture"+suffix)
	command := exec.Command("go", "build", "-o", path, "./internal/cmd/testdata/daemonfixture")
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return path
}

func startDaemonFixture(t *testing.T, fixture, home, mode string) {
	t.Helper()
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(fixture, "-home", canonicalHome, "-mode", mode, "-ready", marker)
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond)
}

func TestInventorySubprocessPreservesGhosthubJSONAndTrustBehavior(t *testing.T) {
	binary, cleanupBinary := buildDaemonTestBinaries(t)
	home := newDaemonTestHome(t, validDaemonConfig)
	registerDaemonCleanup(t, cleanupBinary, home)
	repository := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repository).Run())
	require.NoError(t, os.WriteFile(
		filepath.Join(repository, ".kwt.toml"),
		[]byte("[naming]\ntemplate = 'untrusted/{{.Branch}}'\n"),
		0o600,
	))

	stdout, stderr, err := runInventoryCommand(t, binary, home, repository, "list", "--json")
	require.NoError(t, err, "stderr=%s", stderr)
	assert.True(t, json.Valid(stdout))
	assert.Equal(t, byte('['), bytes.TrimSpace(stdout)[0])
	assert.Contains(t, string(stderr), "kwt: skipping untrusted local config ")
	assert.Contains(t, string(stderr), " (non-interactive session)\n")

	stdout, stderr, err = runInventoryCommand(t, binary, home, repository, "projects", "--json")
	require.NoError(t, err, "stderr=%s", stderr)
	assert.Equal(t, "[]\n", string(stdout))
}

func TestInventorySubprocessExpandsGlobalPathsForEachClient(t *testing.T) {
	binary, cleanupBinary := buildDaemonTestBinaries(t)
	home := newDaemonTestHome(t, validDaemonConfig+`
[[projects]]
repository = "github.com/acme/selected"
name = "selected"
path = "$KWT_TEST_PROJECT"
`)
	registerDaemonCleanup(t, cleanupBinary, home)
	firstRepository := filepath.Join(t.TempDir(), "first")
	secondRepository := filepath.Join(t.TempDir(), "second")
	for _, repository := range []string{firstRepository, secondRepository} {
		require.NoError(t, exec.Command("git", "init", repository).Run())
	}
	firstRepository, err := filepath.EvalSymlinks(firstRepository)
	require.NoError(t, err)
	secondRepository, err = filepath.EvalSymlinks(secondRepository)
	require.NoError(t, err)
	directory := t.TempDir()

	t.Setenv("KWT_TEST_PROJECT", firstRepository)
	first, stderr, err := runInventoryCommand(t, binary, home, directory, "projects", "--json")
	require.NoError(t, err, "stderr=%s", stderr)
	t.Setenv("KWT_TEST_PROJECT", secondRepository)
	second, stderr, err := runInventoryCommand(t, binary, home, directory, "projects", "--json")
	require.NoError(t, err, "stderr=%s", stderr)

	var firstProjects, secondProjects []models.Project
	require.NoError(t, json.Unmarshal(first, &firstProjects))
	require.NoError(t, json.Unmarshal(second, &secondProjects))
	require.Len(t, firstProjects, 1)
	require.Len(t, secondProjects, 1)
	assert.Equal(t, firstRepository, firstProjects[0].Path)
	assert.Equal(t, secondRepository, secondProjects[0].Path)
}

func TestProjectsRemoveIsVisibleToDaemonInventory(t *testing.T) {
	binary, cleanupBinary := buildDaemonTestBinaries(t)
	repository := filepath.Join(t.TempDir(), "widget")
	require.NoError(t, exec.Command("git", "init", repository).Run())
	repository, err := filepath.EvalSymlinks(repository)
	require.NoError(t, err)
	home := newDaemonTestHome(t, validDaemonConfig+`
[[projects]]
repository = "github.com/acme/widget"
name = "widget"
path = "`+filepath.ToSlash(repository)+`"
`)
	registerDaemonCleanup(t, cleanupBinary, home)
	directory := t.TempDir()

	before, stderr, err := runInventoryCommand(
		t, binary, home, directory, "projects", "--json",
	)
	require.NoError(t, err, "stderr=%s", stderr)
	var projects []models.Project
	require.NoError(t, json.Unmarshal(before, &projects))
	require.Len(t, projects, 1)

	removed, stderr, err := runInventoryCommand(
		t, binary, home, directory,
		"projects", "remove", repository, "--json",
	)
	require.NoError(t, err, "stderr=%s", stderr)
	var response struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(removed, &response))
	assert.Equal(t, "unregistered", response.Status)

	after, stderr, err := runInventoryCommand(
		t, binary, home, directory, "projects", "--json",
	)
	require.NoError(t, err, "stderr=%s", stderr)
	assert.Equal(t, "[]\n", string(after))
}

func TestInventorySubprocessDaemonFailuresNeverUseSSHExit255(t *testing.T) {
	binary, _ := buildDaemonTestBinaries(t)
	fixture := buildDaemonFixture(t)
	for _, test := range []struct {
		mode string
		code service.Code
	}{
		{mode: "unresponsive", code: service.DaemonUnresponsive},
		{mode: "incompatible", code: service.DaemonIncompatible},
		{mode: "draining", code: service.DaemonDraining},
		{mode: "inventory_failed", code: service.InventoryFailed},
	} {
		t.Run(test.mode, func(t *testing.T) {
			home := newDaemonTestHome(t, validDaemonConfig)
			repository := filepath.Join(t.TempDir(), "repo")
			require.NoError(t, exec.Command("git", "init", repository).Run())
			startDaemonFixture(t, fixture, home, test.mode)

			stdout, stderr, err := runInventoryCommand(t, binary, home, repository, "list", "--json")
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "stderr=%s", stderr)
			assert.Equal(t, 1, exitErr.ExitCode(), "stderr=%s", stderr)
			assert.NotEqual(t, 255, exitErr.ExitCode())
			var envelope jsonErrorEnvelope
			require.NoError(t, json.Unmarshal(stdout, &envelope), "stdout=%s stderr=%s", stdout, stderr)
			assert.Equal(t, test.code, envelope.Error.Code)
			assert.NotEmpty(t, envelope.Error.Message)
			assert.NotContains(t, string(stderr), `{"error":`)
		})
	}
}
