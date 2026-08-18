package cmd

import (
	"fmt"
	"io"
	"os"

	"go.kenn.io/kwt/internal/tmux"
)

func tmuxDiagnosticReporter(writer io.Writer) func(error) {
	if writer == nil {
		writer = os.Stderr
	}
	return func(err error) {
		_, _ = fmt.Fprintf(writer, "warning: %v\n", err)
	}
}

func newCommandWorkspaceSessions(
	stripNames []string,
	stderr io.Writer,
) *tmux.WorkspaceSessions {
	return tmux.NewWorkspaceSessions(tmux.WorkspaceSessionsOptions{
		StripNames:       stripNames,
		ReportDiagnostic: tmuxDiagnosticReporter(stderr),
	})
}
