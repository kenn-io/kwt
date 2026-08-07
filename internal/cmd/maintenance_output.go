package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/maintenance"
	"golang.org/x/term"
)

type maintenanceCommandError struct {
	code int
	err  error
}

func (e *maintenanceCommandError) Error() string { return e.err.Error() }
func (e *maintenanceCommandError) Unwrap() error { return e.err }
func (e *maintenanceCommandError) ExitCode() int { return e.code }

type maintenanceErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type maintenanceErrorEnvelope struct {
	SchemaVersion int                  `json:"schema_version"`
	Command       string               `json:"command"`
	Error         maintenanceErrorBody `json:"error"`
}

func renderDoctorReport(
	cmd *cobra.Command,
	report maintenance.Report,
	jsonOutput bool,
) error {
	if jsonOutput {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	_, err := io.WriteString(cmd.OutOrStdout(), renderDoctorHuman(report, doctorOptionsForCommand(cmd)))
	return err
}

func doctorOptionsForCommand(cmd *cobra.Command) doctorRenderOptions {
	options := doctorRenderOptions{Width: 100, HomeDir: userHomeDir(), TempDir: os.TempDir()}
	output, ok := cmd.OutOrStdout().(*os.File)
	if !ok || !term.IsTerminal(int(output.Fd())) {
		return options
	}
	if width, _, err := term.GetSize(int(output.Fd())); err == nil && width > 0 {
		options.Width = width
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	options.Color = !noColor && os.Getenv("CLICOLOR") != "0"
	return options
}

func userHomeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

func writeMaintenanceError(
	cmd *cobra.Command,
	command string,
	code string,
	message string,
	exitCode int,
	jsonOutput bool,
) error {
	cmd.Root().SilenceUsage = true
	cmd.Root().SilenceErrors = true
	if jsonOutput {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(maintenanceErrorEnvelope{
			SchemaVersion: maintenance.SchemaVersion,
			Command:       command,
			Error: maintenanceErrorBody{
				Code: code, Message: message, Retryable: false,
			},
		})
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "kwt %s: %s: %s\n", command, code, message)
	return &maintenanceCommandError{code: exitCode, err: fmt.Errorf("%s", message)}
}
