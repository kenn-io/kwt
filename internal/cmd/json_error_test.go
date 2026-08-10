package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestWriteCommandFailureUsesSharedJSONEnvelope(t *testing.T) {
	root := &cobra.Command{Use: "kwt"}
	cmd := &cobra.Command{Use: "list"}
	root.AddCommand(cmd)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	descriptor := service.Descriptor{
		Code: service.DaemonDraining, Message: "the kwt daemon is draining", Retryable: true,
		Details: map[string]any{"drain_deadline": "2026-08-10T01:02:03Z"},
	}

	err := writeCommandFailure(cmd, descriptor, 1, true, "list")

	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 1, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, descriptor.Code, envelope.Error.Code)
	assert.Equal(t, descriptor.Message, envelope.Error.Message)
	assert.Equal(t, descriptor.Retryable, envelope.Error.Retryable)
	assert.Equal(t, descriptor.Details, envelope.Error.Details)
	assert.Equal(t, "kwt list: daemon_draining: the kwt daemon is draining\n", stderr.String())
	assert.True(t, root.SilenceUsage)
	assert.True(t, root.SilenceErrors)
}

func TestWriteCommandFailureOmitsEmptyDetails(t *testing.T) {
	cmd := &cobra.Command{Use: "kwt"}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})

	_ = writeCommandFailure(cmd, service.Descriptor{
		Code: service.InvalidRequest, Message: "invalid request",
	}, 2, true, "projects")

	assert.NotContains(t, stdout.String(), "details")
}
