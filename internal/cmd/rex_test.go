package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
