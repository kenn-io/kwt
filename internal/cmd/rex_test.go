package cmd

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/rex"
)

func TestRexListingTreatsAMissingOrStoppedRexAsEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Rex servers run on macOS, Linux, and FreeBSD")
	}
	t.Setenv("REX_SERVER", "")

	listing, err := rexListing(t.Context(), []string{filepath.Join(t.TempDir(), "missing")})
	require.NoError(t, err)
	assert.Equal(t, "not_installed", listing.Status)
	assert.NotNil(t, listing.Terminals, "JSON lists no terminals rather than null")

	stopped := filepath.Join(t.TempDir(), "rex")
	require.NoError(t, os.WriteFile(stopped, []byte("#!/bin/sh\nexit 1\n"), 0o700)) //nolint:gosec // Test executable.
	listing, err = rexListing(t.Context(), []string{stopped})
	require.NoError(t, err)
	assert.Equal(t, "not_running", listing.Status)
	assert.Empty(t, listing.Terminals)
}

func TestRexListingTreatsAStoppedServerSocketAsNotRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Rex servers run on macOS, Linux, and FreeBSD")
	}
	// Unix socket paths are short, so use a short directory.
	dir, err := os.MkdirTemp("", "rex")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	missing := filepath.Join(dir, "missing.sock")
	refusing := filepath.Join(dir, "stopped.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: refusing, Net: "unix"})
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	require.NoError(t, listener.Close())

	for _, socket := range []string{missing, refusing} {
		t.Setenv("REX_SERVER", "unix://"+socket)
		listing, err := rexListing(t.Context(), nil)
		require.NoError(t, err, socket)
		assert.Equal(t, "not_running", listing.Status, socket)
		assert.Empty(t, listing.Terminals)
	}
}

func TestRexControlRefusesAnythingButAWindowID(t *testing.T) {
	t.Setenv("REX_SERVER", "")
	for _, id := range []string{"block:01a1", "--help", "window:", "window:a b"} {
		err := runRexControl(t.Context(), id, nil, strings.NewReader(""), io.Discard)
		require.ErrorContains(t, err, "is not a Rex window ID", id)
	}
}

func TestRexControlReportsAStoppedServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Rex servers run on macOS, Linux, and FreeBSD")
	}
	dir, err := os.MkdirTemp("", "rex")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("REX_SERVER", "unix://"+filepath.Join(dir, "missing.sock"))

	err = runRexControl(t.Context(), "window:01a1", nil, strings.NewReader(""), io.Discard)
	require.ErrorIs(t, err, rex.ErrNotRunning)
}

func TestRexNewRefusesAnythingButASessionID(t *testing.T) {
	t.Setenv("REX_SERVER", "")
	for _, id := range []string{"window:01a1", "--help", "session:", "session:a b"} {
		err := runRexNew(t.Context(), rex.NewWindowOptions{SessionID: id}, nil, io.Discard)
		require.ErrorContains(t, err, "is not a Rex session ID", id)
	}
}

func TestRexNewReportsAStoppedServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Rex servers run on macOS, Linux, and FreeBSD")
	}
	dir, err := os.MkdirTemp("", "rex")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("REX_SERVER", "unix://"+filepath.Join(dir, "missing.sock"))

	err = runRexNew(t.Context(), rex.NewWindowOptions{}, nil, io.Discard)
	require.ErrorIs(t, err, rex.ErrNotRunning)
}
