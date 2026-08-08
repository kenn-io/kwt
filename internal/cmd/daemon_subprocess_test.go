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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
)

const validDaemonConfig = `[daemon]
idle_timeout = "2h"
auto_restart = "newer"
replacement_grace = "200ms"
`

func buildDaemonTestBinaries(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	dir := t.TempDir()
	build := func(name, version, revision string) string {
		path := filepath.Join(dir, name+suffix)
		command := exec.Command(
			"go",
			"build",
			"-ldflags",
			"-X go.kenn.io/kwt/internal/cmd.version="+version+
				" -X go.kenn.io/kwt/internal/cmd.commit="+revision,
			"-o",
			path,
			"./cmd/kwt",
		)
		command.Dir = root
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		return path
	}
	return build("kwt-old", "v1.0.0", "old"),
		build("kwt-new", "v1.1.0", "new")
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
