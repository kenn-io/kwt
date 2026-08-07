package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Long:  `Show detailed version information including build details.`,
	Run: func(cmd *cobra.Command, args []string) {
		showVersion()
	},
}

func showVersion() {
	build := currentBuildInfo()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		// Fallback to compile-time variables
		fmt.Printf("kwt version %s\n", build.Version)
		fmt.Printf("  commit: %s\n", build.Revision)
		fmt.Printf("  built: %s\n", build.Date)
		fmt.Printf("  go: %s\n", runtime.Version())
		fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		return
	}

	// Use build info from runtime
	fmt.Printf("kwt version %s\n", build.Version)

	vcsModified := false

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.modified":
			vcsModified = setting.Value == "true"
		}
	}

	if build.Revision != "" {
		fmt.Printf("  commit: %s\n", build.Revision)
		if vcsModified {
			fmt.Printf("  modified: true\n")
		}
	}

	if build.Date != "" {
		fmt.Printf("  built: %s\n", build.Date)
	}

	fmt.Printf("  go: %s\n", info.GoVersion)
	fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)

	// Show module information
	if info.Main.Path != "" {
		fmt.Printf("  module: %s\n", info.Main.Path)
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			fmt.Printf("  module version: %s\n", info.Main.Version)
		}
	}
}
