package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
)

func TestTmuxDiagnosticReporterWritesWarning(t *testing.T) {
	var output bytes.Buffer

	tmuxDiagnosticReporter(&output)(errors.New("default tmux server unavailable"))

	assert.Equal(t, "warning: default tmux server unavailable\n", output.String())
}

func TestFormatTmuxSessionLabelDisambiguatesEndpoints(t *testing.T) {
	assert.Equal(t, "run/test [kwt]", formatTmuxSessionLabel(&tmux.Session{
		SocketName: tmux.KWTServerSocketName,
		Context:    "run", Identifier: "test",
	}))
	assert.Equal(t, "run/test [default]", formatTmuxSessionLabel(&tmux.Session{
		Context: "run", Identifier: "test",
	}))
}

func TestTmuxSessionRecordSerializesEndpointWithAttachMode(t *testing.T) {
	started := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	session := &tmux.Session{
		SessionName: "kwt-run-test-20260102030405",
		SocketName:  tmux.KWTServerSocketName,
		ID:          "session-id",
		Context:     "run",
		Identifier:  "test",
		WorkingDir:  "/work/test",
		Command:     "make test",
		StartTime:   started,
		HistorySize: 50000,
		Metadata:    map[string]string{"created_by": "kwt tmux run"},
	}

	stdout, err := captureListStdout(t, func() error {
		return outputSessionsJSON([]*tmux.Session{session})
	})
	require.NoError(t, err)
	assert.JSONEq(t, `[{
		"id":"session-id",
		"session_name":"kwt-run-test-20260102030405",
		"context":"run",
		"identifier":"test",
		"working_dir":"/work/test",
		"command":"make test",
		"start_time":"2026-01-02T03:04:05Z",
		"history_size":50000,
		"metadata":{"created_by":"kwt tmux run"},
		"tmux_socket_name":"kwt",
		"tmux_attach_mode":"direct"
	}]`, stdout)

	var records []tmuxSessionRecord
	require.NoError(t, json.Unmarshal([]byte(stdout), &records))
	require.Len(t, records, 1)
	assert.Equal(t, models.TmuxAttachDirect, records[0].TmuxAttachMode)
}
