package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/git"
)

const (
	maintenanceGitMajor = 2
	maintenanceGitMinor = 31
)

var requireMaintenanceGitVersion = func() error {
	return git.RequireVersion(maintenanceGitMajor, maintenanceGitMinor)
}

func checkMaintenanceGitVersion(
	cmd *cobra.Command,
	command string,
	jsonOutput bool,
) error {
	if err := requireMaintenanceGitVersion(); err != nil {
		return writeMaintenanceError(
			cmd,
			command,
			"unsupported_git_version",
			err.Error(),
			2,
			jsonOutput,
		)
	}
	return nil
}
