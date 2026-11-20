//go:build !windows

package tmuxrestore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTmuxRefusesASocketAnotherAccountControls(t *testing.T) {
	tm := privateSocket(t)
	startServer(t, tm, "-s", "work")
	// Seen from another account, this server's socket and directory are
	// someone else's.
	real := currentUID
	t.Cleanup(func() { currentUID = real })
	currentUID = func() uint32 { return real() + 1 }

	_, err := tm.Run(t.Context(), "list-sessions")
	require.ErrorContains(t, err, "another account")
	state, _, err := tm.Probe(t.Context())
	assert.Equal(t, ServerUnknown, state, "nothing is concluded about a server kwt will not talk to")
	require.Error(t, err)

	if os.Geteuid() != 0 {
		// Root owns the shared directories, so its directories are trusted.
		unbound := Tmux{Executable: tm.Executable, Socket: Socket{Path: filepath.Join(filepath.Dir(tm.Socket.Path), "new")}}
		_, err = unbound.Run(t.Context(), "list-sessions")
		require.ErrorContains(t, err, "another account", "a server is not started in someone else's directory")
	}

	currentUID = real
	_, err = tm.Run(t.Context(), "list-sessions")
	require.NoError(t, err)
}

func TestTmuxRefusesASymlinkedSocket(t *testing.T) {
	// The link is ours, but what it points to can change hands.
	tm := privateSocket(t)
	startServer(t, tm, "-s", "work")
	link := filepath.Join(filepath.Dir(tm.Socket.Path), "link")
	require.NoError(t, os.Symlink(tm.Socket.Path, link))

	_, err := Tmux{Executable: tm.Executable, Socket: Socket{Path: link}}.Run(t.Context(), "list-sessions")

	require.ErrorContains(t, err, "symbolic link")
}
