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
	"go.kenn.io/kwt/internal/config"
	publicworktree "go.kenn.io/kwt/worktree"
)

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
	var gotRequest publicworktree.Request
	queryListInventory = func(
		_ context.Context,
		request publicworktree.Request,
		_ bool,
		_ io.Writer,
	) (publicworktree.Result, error) {
		gotRequest = request
		return publicworktree.Result{Snapshot: publicworktree.Snapshot{Entries: []publicworktree.Entry{{
			Path: repository, Branch: "main",
			Repository:  publicworktree.Repository{FullPath: "github.com/acme/repo"},
			SessionName: "kwt-workspace-repo-main",
		}}}}, nil
	}
	previousJSON, previousGlobal := listJSON, listGlobal
	listJSON, listGlobal = true, false
	t.Cleanup(func() { listJSON, listGlobal = previousJSON, previousGlobal })

	stdout, err := captureListStdout(t, func() error { return runList(listCmd, nil) })
	require.NoError(t, err)
	assert.Equal(t, publicworktree.ViewRepository, gotRequest.View)
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
      "session_name": "kwt-workspace-repo-main"
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
	queryListInventory = func(context.Context, publicworktree.Request, bool, io.Writer) (publicworktree.Result, error) {
		return publicworktree.Result{Snapshot: publicworktree.Snapshot{Entries: []publicworktree.Entry{}}}, nil
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
