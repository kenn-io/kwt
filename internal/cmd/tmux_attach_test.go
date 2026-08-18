package cmd

import (
	"context"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/tmux"
)

type recordingTmuxAttachSessionManager struct {
	sessions []*tmux.Session
	attached *tmux.Session
}

func (*recordingTmuxAttachSessionManager) CreateSession(
	context.Context,
	tmux.SessionOptions,
) (*tmux.Session, error) {
	return nil, nil
}

func (m *recordingTmuxAttachSessionManager) ListSessions() ([]*tmux.Session, error) {
	return m.sessions, nil
}

func (*recordingTmuxAttachSessionManager) GetSession(string) (*tmux.Session, error) {
	return nil, nil
}

func (*recordingTmuxAttachSessionManager) KillSession(string) error { return nil }

func (*recordingTmuxAttachSessionManager) KillSessionDirect(*tmux.Session) error {
	return nil
}

func (*recordingTmuxAttachSessionManager) AttachSession(string) error { return nil }

func (m *recordingTmuxAttachSessionManager) AttachSessionDirect(session *tmux.Session) error {
	m.attached = session
	return nil
}

func (*recordingTmuxAttachSessionManager) HasSession(string) bool { return false }

func TestRunTmuxAttachPassesConfiguredProtectedNames(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	t.Setenv("CUSTOM_FLEET_TOKEN", "secret")
	viper.Set("fleet.token_env", "CUSTOM_FLEET_TOKEN")
	manager := &recordingTmuxAttachSessionManager{sessions: []*tmux.Session{{
		SessionName: "workspace",
	}}}
	var protectedNames []string
	previousManager := newTmuxAttachSessionManager
	previousInteractive := tmuxAttachInteractive
	t.Cleanup(func() {
		newTmuxAttachSessionManager = previousManager
		tmuxAttachInteractive = previousInteractive
	})
	newTmuxAttachSessionManager = func(
		_ *tmux.SessionConfig,
		names ...string,
	) tmux.SessionManagerInterface {
		protectedNames = append([]string(nil), names...)
		return manager
	}
	tmuxAttachInteractive = false

	err := runTmuxAttach(tmuxAttachCmd, []string{"workspace"})

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"KWT_GITHUB_TOKEN",
		"KWT_FLEET_TOKEN",
		"CUSTOM_FLEET_TOKEN",
	}, protectedNames)
	assert.Same(t, manager.sessions[0], manager.attached)
}
