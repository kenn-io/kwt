package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestOperationCallbacksWriteProgressBeforeCompletion(t *testing.T) {
	writes := make(chan string, 1)
	command := &cobra.Command{}
	command.SetErr(&observedOperationWriter{writes: writes})
	callbacks := operationCallbacks(command, nil)

	require.NoError(t, callbacks.Event(service.OperationEvent{
		Kind: service.OperationEventProgress, Message: "resolving route",
	}))
	select {
	case output := <-writes:
		assert.Equal(t, "resolving route\n", output)
	case <-time.After(time.Second):
		t.Fatal("progress was buffered instead of written immediately")
	}
}

func TestOperationCallbacksKeepMachineOutputSeparate(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	callbacks := operationCallbacks(command, nil)

	require.NoError(t, callbacks.Event(service.OperationEvent{
		Kind: service.OperationEventWarning, Message: "connection is slow",
	}))
	assert.Empty(t, stdout.String())
	assert.Equal(t, "kwt: connection is slow\n", stderr.String())
}

func TestOperationCallbacksRequireExplicitPromptHandler(t *testing.T) {
	command := &cobra.Command{}
	callbacks := operationCallbacks(command, nil)
	_, err := callbacks.Prompt(context.Background(), service.OperationPrompt{
		ID: "prompt-1", Kind: "password", Message: "Password:", Sensitive: true,
	})
	assert.True(t, service.IsCode(err, service.InteractionRequired), err)
}

func TestOperationCallbacksNeverRenderPromptResponse(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	callbacks := operationCallbacks(command, func(
		context.Context,
		service.OperationPrompt,
	) (string, error) {
		return "fleet secret", nil
	})

	value, err := callbacks.Prompt(context.Background(), service.OperationPrompt{
		ID: "prompt-1", Kind: "password", Message: "Password:", Sensitive: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "fleet secret", value)
	assert.False(t, strings.Contains(stdout.String()+stderr.String(), "fleet secret"))
}

type observedOperationWriter struct {
	writes chan<- string
}

func (w *observedOperationWriter) Write(value []byte) (int, error) {
	w.writes <- string(value)
	return len(value), nil
}
