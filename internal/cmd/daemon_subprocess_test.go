package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
)

const validDaemonConfig = `[daemon]
idle_timeout = "2h"
auto_restart = "newer"
replacement_grace = "200ms"
`

func buildDaemonTestBinaries(t *testing.T) (string, string) {
	t.Helper()
	return buildDaemonTestBinary(t, daemonTestBuild{
			Name: "kwt-old", Version: "v1.0.0", Revision: strings.Repeat("a", 40),
		}), buildDaemonTestBinary(t, daemonTestBuild{
			Name: "kwt-new", Version: "v1.1.0", Revision: strings.Repeat("b", 40),
		})
}

type daemonTestBuild struct {
	Name         string
	Version      string
	Revision     string
	RevisionTime string
}

func buildDaemonTestBinary(t *testing.T, build daemonTestBuild) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	dir := t.TempDir()
	path := filepath.Join(dir, build.Name+suffix)
	command := exec.Command(
		"go",
		"build",
		"-ldflags",
		"-X go.kenn.io/kwt/internal/cmd.version="+build.Version+
			" -X go.kenn.io/kwt/internal/cmd.commit="+build.Revision+
			" -X go.kenn.io/kwt/internal/cmd.revisionTime="+build.RevisionTime,
		"-o",
		path,
		"./cmd/kwt",
	)
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return path
}

func newDaemonTestHome(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte(body),
		0o600,
	))
	return home
}

func runDaemonCommand(
	t *testing.T,
	binary string,
	home string,
	args ...string,
) ([]byte, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = append(os.Environ(), "KWT_HOME="+home)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func requireCommandSuccess(
	t *testing.T,
	binary string,
	home string,
	args ...string,
) {
	t.Helper()
	stdout, stderr, err := runDaemonCommand(t, binary, home, args...)
	require.NoError(t, err, "stdout=%s stderr=%s", stdout, stderr)
}

func daemonStatus(t *testing.T, binary, home string) kwtdaemon.Status {
	t.Helper()
	stdout, stderr, err := runDaemonCommand(
		t,
		binary,
		home,
		"daemon",
		"status",
		"--json",
	)
	require.NoError(t, err, "stderr=%s", stderr)
	var status kwtdaemon.Status
	require.NoError(t, json.Unmarshal(stdout, &status))
	return status
}

func daemonRuntimeRecords(t *testing.T, home string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, "runtime", "kwt.*.json"))
	require.NoError(t, err)
	return paths
}

func registerDaemonCleanup(t *testing.T, binary, home string) {
	t.Helper()
	t.Cleanup(func() {
		_, stderr, err := runDaemonCommand(t, binary, home, "daemon", "stop")
		if err != nil && len(daemonRuntimeRecords(t, home)) > 0 {
			t.Errorf(
				"daemon cleanup failed without signaling an unverified PID: %v: %s",
				err,
				stderr,
			)
		}
	})
}

func TestProjectsRemoveMissingExpectedRepositoryReturnsJSON(t *testing.T) {
	binary := buildDaemonTestBinary(t, daemonTestBuild{
		Name: "kwt-project-remove-validation", Version: "v1.5.0",
		Revision: strings.Repeat("e", 40),
	})
	home := newDaemonTestHome(t, validDaemonConfig)

	stdout, stderr, err := runDaemonCommand(
		t, binary, home,
		"projects", "remove", filepath.Join(t.TempDir(), "missing"), "--json",
	)

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "stderr=%s", stderr)
	assert.Equal(t, 2, exitErr.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout, &envelope))
	assert.Equal(t, service.InvalidRequest, envelope.Error.Code)
}

func TestDaemonSubprocessGuardedProjectRemoval(t *testing.T) {
	binary := buildDaemonTestBinary(t, daemonTestBuild{
		Name: "kwt-guarded-project", Version: "v1.5.0",
		Revision: strings.Repeat("e", 40),
	})
	credentialCanary := "guarded-project-secret"
	exactPath := filepath.Join(t.TempDir(), "missing ")
	home := newDaemonTestHome(t, validDaemonConfig+fmt.Sprintf(`
[[projects]]
repository = "https://user:%s@github.com/acme/widget.git"
name = "widget"
path = %q
`, credentialCanary, exactPath))
	registerDaemonCleanup(t, binary, home)
	var observed strings.Builder
	run := func(home string, args ...string) ([]byte, []byte, error) {
		stdout, stderr, err := runDaemonCommand(t, binary, home, args...)
		observed.Write(stdout)
		observed.Write(stderr)
		return stdout, stderr, err
	}

	stdout, stderr, err := run(
		home, "projects", "--json",
	)
	require.NoError(t, err, "stderr=%s", stderr)
	var projects []kwt.Project
	require.NoError(t, json.Unmarshal(stdout, &projects))
	require.Len(t, projects, 1)
	assert.Equal(t, exactPath, projects[0].Path)
	assert.Equal(t, "github.com/acme/widget", projects[0].Repository)
	identity := projects[0].Repository
	fingerprint := projects[0].RegistrationFingerprint
	assert.NotEmpty(t, fingerprint)

	stdout, _, err = run(
		home,
		"projects", "remove", strings.TrimSuffix(exactPath, " "),
		"--expected-repository", identity,
		"--expected-registration", fingerprint, "--json",
	)
	require.Error(t, err)
	var missing jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout, &missing))
	assert.Equal(t, service.ProjectNotFound, missing.Error.Code)

	stdout, _, err = run(
		home,
		"projects", "remove", exactPath,
		"--expected-repository", "github.com/acme/other",
		"--expected-registration", fingerprint, "--json",
	)
	require.Error(t, err)
	var changed jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout, &changed))
	assert.Equal(t, service.RegistrationChanged, changed.Error.Code)

	stdout, stderr, err = run(
		home,
		"projects", "remove", exactPath,
		"--expected-repository", identity,
		"--expected-registration", fingerprint, "--json",
	)
	require.NoError(t, err, "stderr=%s", stderr)
	var removed projectMutationResult
	require.NoError(t, json.Unmarshal(stdout, &removed))
	assert.Equal(t, "unregistered", removed.Status)
	assert.Equal(t, exactPath, removed.Project.Path)
	stdout, stderr, err = run(
		home, "projects", "--json",
	)
	require.NoError(t, err, "stderr=%s", stderr)
	require.NoError(t, json.Unmarshal(stdout, &projects))
	assert.Empty(t, projects)

	corruptPath := filepath.Join(t.TempDir(), "unavailable")
	corruptHome := newDaemonTestHome(t, validDaemonConfig+fmt.Sprintf(`
[[projects]]
repository = "github.com/acme/corrupt"
name = "corrupt"
path = %q
`, corruptPath))
	require.NoError(t, os.WriteFile(
		filepath.Join(corruptHome, "pull-requests.json"), []byte("{"), 0o600,
	))
	registerDaemonCleanup(t, binary, corruptHome)
	stdout, stderr, err = run(corruptHome, "projects", "--json")
	require.NoError(t, err, "stderr=%s", stderr)
	var corruptProjects []kwt.Project
	require.NoError(t, json.Unmarshal(stdout, &corruptProjects))
	require.Len(t, corruptProjects, 1)
	stdout, _, err = run(
		corruptHome,
		"projects", "remove", corruptPath,
		"--expected-repository", "github.com/acme/corrupt",
		"--expected-registration", corruptProjects[0].RegistrationFingerprint,
		"--json",
	)
	require.Error(t, err)
	var incomplete jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout, &incomplete))
	assert.Equal(
		t, service.ProtectedEndpointInventoryIncomplete, incomplete.Error.Code,
	)

	for _, daemonHome := range []string{home, corruptHome} {
		logData, readErr := os.ReadFile(filepath.Join(daemonHome, "daemon.log"))
		if readErr == nil {
			observed.Write(logData)
		} else {
			require.ErrorIs(t, readErr, os.ErrNotExist)
		}
	}
	assert.NotContains(t, observed.String(), credentialCanary)
}

func TestDaemonSubprocessConcurrentStartStatusRestartStop(t *testing.T) {
	oldBinary, newBinary := buildDaemonTestBinaries(t)
	home := newDaemonTestHome(t, validDaemonConfig)
	registerDaemonCleanup(t, newBinary, home)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			stdout, stderr, err := runDaemonCommand(
				t,
				oldBinary,
				home,
				"daemon",
				"start",
			)
			if err != nil {
				err = fmt.Errorf("%w: stdout=%s stderr=%s", err, stdout, stderr)
			}
			errors <- err
		}()
	}
	for range 2 {
		require.NoError(t, <-errors)
	}
	started := daemonStatus(t, oldBinary, home)
	require.Positive(t, started.PID)
	requireCommandSuccess(t, oldBinary, home, "daemon", "restart")
	restarted := daemonStatus(t, oldBinary, home)
	require.Positive(t, restarted.PID)
	assert.NotEqual(t, started.PID, restarted.PID)
	requireCommandSuccess(t, oldBinary, home, "daemon", "stop")
	stopped := daemonStatus(t, oldBinary, home)
	assert.Equal(t, kwtdaemon.State("stopped"), stopped.State)
	assert.Empty(t, daemonRuntimeRecords(t, home))
}

func TestDaemonSubprocessNewestCompatibleBinaryWins(t *testing.T) {
	oldBinary, newBinary := buildDaemonTestBinaries(t)
	home := newDaemonTestHome(t, validDaemonConfig)
	registerDaemonCleanup(t, newBinary, home)
	requireCommandSuccess(t, oldBinary, home, "daemon", "start")
	old := daemonStatus(t, oldBinary, home)
	assert.Equal(t, "v1.0.0", old.Version)

	requireCommandSuccess(t, newBinary, home, "daemon", "start")
	upgraded := daemonStatus(t, newBinary, home)
	assert.Equal(t, "v1.1.0", upgraded.Version)
	assert.NotEqual(t, old.PID, upgraded.PID)

	requireCommandSuccess(t, oldBinary, home, "daemon", "start")
	stillNew := daemonStatus(t, oldBinary, home)
	assert.Equal(t, upgraded.PID, stillNew.PID)
}

func TestDaemonSubprocessSHARevisionOrderingAndEqualTimeOverride(t *testing.T) {
	older := buildDaemonTestBinary(t, daemonTestBuild{
		Name: "kwt-sha-older", Version: "sha-older", Revision: strings.Repeat("a", 40),
		RevisionTime: "2026-08-09T12:00:00Z",
	})
	newer := buildDaemonTestBinary(t, daemonTestBuild{
		Name: "kwt-sha-newer", Version: "sha-newer", Revision: strings.Repeat("b", 40),
		RevisionTime: "2026-08-09T12:00:01Z",
	})
	equalLeft := buildDaemonTestBinary(t, daemonTestBuild{
		Name: "kwt-sha-equal-left", Version: "sha-left", Revision: strings.Repeat("c", 40),
		RevisionTime: "2026-08-09T12:00:02Z",
	})
	equalRight := buildDaemonTestBinary(t, daemonTestBuild{
		Name: "kwt-sha-equal-right", Version: "sha-right", Revision: strings.Repeat("d", 40),
		RevisionTime: "2026-08-09T12:00:02Z",
	})

	orderedHome := newDaemonTestHome(t, validDaemonConfig)
	registerDaemonCleanup(t, newer, orderedHome)
	requireCommandSuccess(t, older, orderedHome, "daemon", "start")
	oldStatus := daemonStatus(t, older, orderedHome)
	_, progress, err := runDaemonCommand(t, newer, orderedHome, "daemon", "start")
	require.NoError(t, err, "stderr=%s", progress)
	assert.Contains(t, string(progress), "daemon draining")
	newStatus := daemonStatus(t, newer, orderedHome)
	assert.Equal(t, strings.Repeat("b", 40), newStatus.Revision)
	assert.Equal(t, "2026-08-09T12:00:01Z", newStatus.RevisionTime)
	assert.NotEqual(t, oldStatus.PID, newStatus.PID)

	requireCommandSuccess(t, older, orderedHome, "daemon", "start")
	reused := daemonStatus(t, older, orderedHome)
	assert.Equal(t, newStatus.PID, reused.PID)
	_, stderr, err := runDaemonCommand(t, older, orderedHome, "daemon", "restart")
	require.Error(t, err)
	assert.Contains(t, string(stderr), "older kwt cannot replace")
	assert.Equal(t, newStatus.PID, daemonStatus(t, newer, orderedHome).PID)

	equalHome := newDaemonTestHome(t, validDaemonConfig)
	registerDaemonCleanup(t, equalRight, equalHome)
	requireCommandSuccess(t, equalLeft, equalHome, "daemon", "start")
	leftStatus := daemonStatus(t, equalLeft, equalHome)
	requireCommandSuccess(t, equalRight, equalHome, "daemon", "start")
	assert.Equal(t, leftStatus.PID, daemonStatus(t, equalRight, equalHome).PID)
	_, stderr, err = runDaemonCommand(t, equalRight, equalHome, "daemon", "restart")
	require.Error(t, err)
	assert.Contains(t, string(stderr), "cannot prove whether it is newer")
	assert.Equal(t, leftStatus.PID, daemonStatus(t, equalLeft, equalHome).PID)

	requireCommandSuccess(t, equalRight, equalHome, "daemon", "stop")
	requireCommandSuccess(t, equalRight, equalHome, "daemon", "start")
	rightStatus := daemonStatus(t, equalRight, equalHome)
	assert.Equal(t, strings.Repeat("d", 40), rightStatus.Revision)
	assert.NotEqual(t, leftStatus.PID, rightStatus.PID)
}

func TestDaemonSubprocessOlderBinaryCannotRestartNewerDaemon(t *testing.T) {
	oldBinary, newBinary := buildDaemonTestBinaries(t)
	home := newDaemonTestHome(t, validDaemonConfig)
	registerDaemonCleanup(t, newBinary, home)
	requireCommandSuccess(t, newBinary, home, "daemon", "start")
	before := daemonStatus(t, newBinary, home)

	_, _, err := runDaemonCommand(t, oldBinary, home, "daemon", "restart")
	require.Error(t, err)
	after := daemonStatus(t, newBinary, home)
	assert.Equal(t, before.PID, after.PID)
	assert.Equal(t, before.Version, after.Version)
}

func TestDaemonSubprocessIdleExitDoesNotNeedAClientProbe(t *testing.T) {
	oldBinary, newBinary := buildDaemonTestBinaries(t)
	home := newDaemonTestHome(t, `[daemon]
idle_timeout = "50ms"
auto_restart = "newer"
replacement_grace = "200ms"
`)
	registerDaemonCleanup(t, newBinary, home)
	requireCommandSuccess(t, oldBinary, home, "daemon", "start")
	deadline := time.Now().Add(2 * time.Second)
	for len(daemonRuntimeRecords(t, home)) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.Empty(t, daemonRuntimeRecords(t, home))
}

func TestDaemonSubprocessForegroundServeRefusesExistingOwner(t *testing.T) {
	oldBinary, newBinary := buildDaemonTestBinaries(t)
	home := newDaemonTestHome(t, validDaemonConfig)
	registerDaemonCleanup(t, newBinary, home)
	requireCommandSuccess(t, oldBinary, home, "daemon", "start")
	before := daemonStatus(t, oldBinary, home)
	_, stderr, err := runDaemonCommand(t, newBinary, home, "serve")
	require.Error(t, err)
	assert.Contains(t, string(stderr), "owner")
	after := daemonStatus(t, oldBinary, home)
	assert.Equal(t, before.PID, after.PID)
}
