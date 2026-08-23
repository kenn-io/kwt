package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/service"
)

type jsonErrorEnvelope struct {
	Error service.Descriptor `json:"error"`
}

type commandFailure struct {
	descriptor service.Descriptor
	exitCode   int
	cause      error
}

func (e *commandFailure) Error() string { return e.descriptor.Message }
func (e *commandFailure) ExitCode() int { return e.exitCode }
func (e *commandFailure) Unwrap() error {
	return service.NewDescriptorError(e.descriptor, e.cause)
}

func writeCommandFailure(
	cmd *cobra.Command,
	descriptor service.Descriptor,
	exitCode int,
	jsonRequested bool,
	prefix string,
) *commandFailure {
	cmd.Root().SilenceUsage = true
	cmd.Root().SilenceErrors = true
	if jsonRequested {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(jsonErrorEnvelope{Error: descriptor})
	}
	_, _ = fmt.Fprintf(
		cmd.ErrOrStderr(),
		"kwt %s: %s: %s\n",
		prefix,
		descriptor.Code,
		descriptor.Message,
	)
	return &commandFailure{descriptor: descriptor, exitCode: exitCode}
}
