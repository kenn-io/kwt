package cmd

import (
	"fmt"
	"io"
	"os"

	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/workspace"
	"go.kenn.io/kwt/pkg/models"
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

func newConfiguredWorkspaceSessions(cfg *models.Config, names []string, stderr io.Writer) *workspace.Sessions {
	return workspace.New(cfg, tmux.WorkspaceSessionsOptions{StripNames: names, ReportDiagnostic: tmuxDiagnosticReporter(stderr)})
}
