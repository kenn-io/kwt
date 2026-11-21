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
		Endpoint:    "unix:///Users/me/Library/Application%20Support/rex/server.sock",
		Version:     "0.1.0-preview.14",
		APIRevision: 12,
	}, server)
	called, err := os.ReadFile(args)
	require.NoError(t, err)
	assert.Equal(t, "whoami\n--json\n--autostart=false\n", string(called))
}

func TestFindServerPrefersREX_SERVER(t *testing.T) {
	for _, endpoint := range []string{
		"unix:///run/user/1000/rex/server.sock",
		"http://build.example.ts.net:7297",
		"https://rex.example.ts.net",
	} {
		t.Setenv("REX_SERVER", endpoint)
		server, err := FindServer(t.Context(), nil)
		require.NoError(t, err)
		assert.Equal(t, Server{Endpoint: endpoint}, server)
	}
}

func TestFindServerReportsWhyNoServerIsAvailable(t *testing.T) {
	t.Setenv("REX_SERVER", "")
	_, err := FindServer(t.Context(), []string{filepath.Join(t.TempDir(), "missing")})
	require.ErrorIs(t, err, ErrNotInstalled)

	stopped := fakeRex(t, "echo 'Error: no Rex server is running' >&2; exit 1\n")
	_, err = FindServer(t.Context(), []string{stopped})
	require.ErrorIs(t, err, ErrNotRunning)

	for _, endpoint := range []string{"tcp://example.com:7000", "ssh://build.example.com", "build.example.com", "http://"} {
		t.Setenv("REX_SERVER", endpoint)
		_, err = FindServer(t.Context(), nil)
		require.ErrorContains(t, err, "http://", endpoint)
	}
}
