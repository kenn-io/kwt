package tmux

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWorkspaceSession(t *testing.T) {
	sm := NewSessionManager(nil)
	info := &SessionInfo{
		Name:           "kwt-wt-kwt-feature-foo-abcd1234",
		Created:        "1700000000",
		WorkingDir:     "/wt",
		CurrentCommand: "zsh",
	}
	got := sm.parseSessionFromTmux(info)
	require.NotNil(t, got)
	assert.Equal(t, "workspace", got.Context)
	assert.Equal(t, "kwt-feature-foo", got.Identifier)
	assert.Equal(t, info.Name, got.SessionName)
	assert.Equal(t, "/wt", got.WorkingDir)
	// StartTime comes from tmux's #{session_created}, parsed by parseCreated.
	assert.Equal(t, time.Unix(1700000000, 0), got.StartTime)
}

func TestParseDirWorkspaceSession(t *testing.T) {
	sm := NewSessionManager(nil)
	info := &SessionInfo{
		Name:    "kwt-workspace-dir-notes-abcd1234",
		Created: "1700000000",
	}

	got := sm.parseSessionFromTmux(info)

	require.NotNil(t, got)
	assert.Equal(t, "workspace", got.Context)
	assert.Equal(t, "dir-notes", got.Identifier)
}

func TestParseLegacySessionStillWorks(t *testing.T) {
	sm := NewSessionManager(nil)
	info := &SessionInfo{
		Name:    "kwt-review-pr123-20240101120000",
		Created: "1700000000",
	}
	got := sm.parseSessionFromTmux(info)
	require.NotNil(t, got)
	assert.Equal(t, "review", got.Context)
	assert.Equal(t, "pr123", got.Identifier)
}

func TestNonKwtSessionIgnored(t *testing.T) {
	sm := NewSessionManager(nil)
	assert.Nil(t, sm.parseSessionFromTmux(&SessionInfo{Name: "random-session"}))
}

func TestParseCreated(t *testing.T) {
	// Valid and whitespace-padded values parse to the exact unix second;
	// a negative value is still a valid (pre-epoch) time.
	assert.Equal(t, time.Unix(1700000000, 0), parseCreated("1700000000"))
	assert.Equal(t, time.Unix(1700000000, 0), parseCreated("  1700000000  "))
	assert.Equal(t, time.Unix(-1, 0), parseCreated("-1"))

	// Empty/garbage/overflow must not panic; they fall back to ~now.
	for _, bad := range []string{"", "garbage", "12.5", "99999999999999999999999999"} {
		assert.WithinDuration(t, time.Now(), parseCreated(bad), time.Minute,
			"parseCreated(%q) must fall back to now without panicking", bad)
	}
}
