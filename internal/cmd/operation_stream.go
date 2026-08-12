package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
)

type operationPromptHandler func(
	context.Context,
	service.OperationPrompt,
) (string, error)

func operationCallbacks(
	command *cobra.Command,
	prompt operationPromptHandler,
) kwtdaemon.OperationCallbacks {
	return kwtdaemon.OperationCallbacks{
		Event: func(event service.OperationEvent) error {
			var message string
			switch event.Kind {
			case service.OperationEventProgress:
				message = event.Message
			case service.OperationEventWarning:
				if event.Message != "" {
					message = "kwt: " + event.Message
				}
			}
			if message == "" {
				return nil
			}
			writer := command.ErrOrStderr()
			if _, err := fmt.Fprintln(writer, message); err != nil {
				return err
			}
			if flusher, ok := writer.(interface{ Flush() error }); ok {
				return flusher.Flush()
			}
			return nil
		},
		Prompt: func(
			ctx context.Context,
			request service.OperationPrompt,
		) (string, error) {
			if prompt == nil {
				return "", service.NewError(
					service.InteractionRequired,
					"operation requires interactive input",
					false,
					map[string]any{"kind": request.Kind},
					nil,
				)
			}
			return prompt(ctx, request)
		},
	}
}
