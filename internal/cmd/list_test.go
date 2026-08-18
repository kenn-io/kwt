package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func TestListedWorktreeCopiesEndpointFields(t *testing.T) {
	got := listedWorktree(kwt.Entry{
		TmuxSocketName: tmux.KWTServerSocketName,
		TmuxAttachMode: models.TmuxAttachDirect,
	})

	assert.Equal(t, tmux.KWTServerSocketName, got.TmuxSocketName)
	assert.Equal(t, models.TmuxAttachDirect, got.TmuxAttachMode)
}

func captureListStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()
	read, write, err := os.Pipe()
	require.NoError(t, err)
	original := os.Stdout
	os.Stdout = write
	defer func() { os.Stdout = original }()
	runErr := run()
	require.NoError(t, write.Close())
	output, err := io.ReadAll(read)
	require.NoError(t, err)
	return string(output), runErr
}

func TestRunListRequestsCurrentInventoryAndPreservesJSONShape(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	home := t.TempDir()
	t.Setenv("KWT_HOME", home)
	require.NoError(t, config.Init())
	repository := t.TempDir()
	t.Chdir(repository)

	originalQuery := queryListInventory
	t.Cleanup(func() { queryListInventory = originalQuery })
	var gotRequest kwt.Request
	queryListInventory = func(
		_ context.Context,
		request kwt.Request,
		_ bool,
		_ io.Writer,
	) (kwt.Result, error) {
		gotRequest = request
		return kwt.Result{Snapshot: kwt.Snapshot{Entries: []kwt.Entry{{
			Path: repository, Branch: "main",
			Repository:     kwt.Repository{FullPath: "github.com/acme/repo"},
			SessionName:    "kwt-workspace-repo-main",
			TmuxSocketName: tmux.KWTServerSocketName,
			TmuxAttachMode: models.TmuxAttachDirect,
		}}}}, nil
	}
	previousJSON, previousGlobal := listJSON, listGlobal
	listJSON, listGlobal = true, false
	t.Cleanup(func() { listJSON, listGlobal = previousJSON, previousGlobal })

	stdout, err := captureListStdout(t, func() error { return runList(listCmd, nil) })
	require.NoError(t, err)
	assert.Equal(t, kwt.ViewRepository, gotRequest.View)
	assert.True(t, gotRequest.RequireCurrent)
	assert.False(t, gotRequest.ForceGlobal)
	assert.True(t, gotRequest.IncludeProtectedSockets)
	encodedRepository, err := json.Marshal(repository)
	require.NoError(t, err)
	assert.JSONEq(t, `[{
	  "path": `+string(encodedRepository)+`,
      "branch": "main",
      "commit_hash": "",
      "is_main": false,
      "created_at": "0001-01-01T00:00:00Z",
      "generation": "",
      "repository": "github.com/acme/repo",
      "session_name": "kwt-workspace-repo-main",
      "tmux_socket_name": "kwt",
      "tmux_attach_mode": "direct"
    }]`, stdout)
}

func TestRunListGlobalEmptyJSONRemainsBareArray(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("KWT_HOME", t.TempDir())
	require.NoError(t, config.Init())
	t.Chdir(t.TempDir())
	originalQuery := queryListInventory
	t.Cleanup(func() { queryListInventory = originalQuery })
	queryListInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{Snapshot: kwt.Snapshot{Entries: []kwt.Entry{}}}, nil
	}
	previousJSON, previousGlobal := listJSON, listGlobal
	listJSON, listGlobal = true, true
	t.Cleanup(func() { listJSON, listGlobal = previousJSON, previousGlobal })

	var output bytes.Buffer
	listCmd.SetOut(&output)
	stdout, err := captureListStdout(t, func() error { return runList(listCmd, nil) })
	require.NoError(t, err)
	assert.Equal(t, "[]\n", stdout)
}

func TestRunListJSONWritesStableDaemonFailure(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("KWT_HOME", t.TempDir())
	require.NoError(t, config.Init())
	t.Chdir(t.TempDir())
	originalQuery := queryListInventory
	t.Cleanup(func() { queryListInventory = originalQuery })
	queryListInventory = func(context.Context, kwt.Request, bool, io.Writer) (kwt.Result, error) {
		return kwt.Result{}, service.NewError(
			service.DaemonDraining,
			"the kwt daemon is draining",
			true,
			map[string]any{"drain_deadline": "2026-08-10T01:02:03Z"},
			nil,
		)
	}
	previousJSON := listJSON
	listJSON = true
	t.Cleanup(func() { listJSON = previousJSON })
	var stdout, stderr bytes.Buffer
	listCmd.SetOut(&stdout)
	listCmd.SetErr(&stderr)

	err := runList(listCmd, nil)

	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 1, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.DaemonDraining, envelope.Error.Code)
	assert.True(t, envelope.Error.Retryable)
	assert.NotEqual(t, byte('['), bytes.TrimSpace(stdout.Bytes())[0])
	assert.Contains(t, stderr.String(), "daemon_draining")
}
