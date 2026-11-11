package rex

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeRex(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Rex servers run on macOS, Linux, and FreeBSD")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "rex")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700)) //nolint:gosec // Test executable.
	return path
}

func TestFindServerReadsWhoamiWithoutStartingAServer(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	rex := fakeRex(t, `printf '%s\n' "$@" > '`+args+`'
printf '%s' '{"endpoint":"unix:///Users/me/Library/Application%20Support/rex/server.sock","rex_version":"0.1.0-preview.14","api_revision":12}'
`)
	t.Setenv("REX_SERVER", "")

	server, err := FindServer(t.Context(), []string{rex})
	require.NoError(t, err)
	assert.Equal(t, Server{
		Socket:      "/Users/me/Library/Application Support/rex/server.sock",
		Version:     "0.1.0-preview.14",
		APIRevision: 12,
	}, server)
	called, err := os.ReadFile(args)
	require.NoError(t, err)
	assert.Equal(t, "whoami\n--json\n--autostart=false\n", string(called))
}

func TestFindServerPrefersREX_SERVER(t *testing.T) {
	t.Setenv("REX_SERVER", "unix:///run/user/1000/rex/server.sock")
	server, err := FindServer(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, "/run/user/1000/rex/server.sock", server.Socket)
}

func TestFindServerReportsWhyNoServerIsAvailable(t *testing.T) {
	t.Setenv("REX_SERVER", "")
	_, err := FindServer(t.Context(), []string{filepath.Join(t.TempDir(), "missing")})
	require.ErrorIs(t, err, ErrNotInstalled)

	stopped := fakeRex(t, "echo 'Error: no Rex server is running' >&2; exit 1\n")
	_, err = FindServer(t.Context(), []string{stopped})
	require.ErrorIs(t, err, ErrNotRunning)

	t.Setenv("REX_SERVER", "tcp://example.com:7000")
	_, err = FindServer(t.Context(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unix://")
}
