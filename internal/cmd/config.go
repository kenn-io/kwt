package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/ui"
)

// configCmd represents the config command.
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Configuration management",
	Long:  `Manage kwt configuration settings.`,
}

// configListCmd represents the config list command.
var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show configuration",
	Long:  `Display all current configuration settings.`,
	Example: `  # Show all configuration
  kwt config list`,
	RunE: runConfigList,
}

// configSetCmd represents the config set command.
var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set configuration value",
	Long: `Set a configuration value.

Configuration keys follow a dot notation format (e.g., worktree.basedir).`,
	Example: `  # Set worktree base directory
  kwt config set worktree.basedir ~/.kwt/worktrees

  # Set naming template
  kwt config set naming.template "{{.Repository}}-{{.Branch}}"

  # Enable/disable colored output
  kwt config set ui.color true`,
	Args:              cobra.ExactArgs(2),
	RunE:              runConfigSet,
	ValidArgsFunction: getConfigKeyCompletions,
}

// configGetCmd represents the config get command.
var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get configuration value",
	Long:  `Get a specific configuration value.`,
	Example: `  # Get worktree base directory
  kwt config get worktree.basedir

  # Get naming template
  kwt config get naming.template`,
	Args:              cobra.ExactArgs(1),
	RunE:              runConfigGet,
	ValidArgsFunction: getConfigKeyCompletions,
}

var configSetLocal bool

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)

	configSetCmd.Flags().BoolVar(&configSetLocal, "local", false, "Write to local config (.kwt.toml) instead of global")
}

func runConfigList(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	printer := ui.New(&cfg.UI)
	settings := config.AllSettings()
	printer.PrintConfig(settings)

	return nil
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

	// Convert string values to appropriate types
	var typedValue any = value
	switch value {
	case "true":
		typedValue = true
	case "false":
		typedValue = false
	default:
		// Try to convert to integer
		if intVal, err := strconv.Atoi(value); err == nil {
			typedValue = intVal
		}
	}

	var err error
	if configSetLocal {
		err = config.SetLocal(key, typedValue)
	} else {
		err = config.SetGlobal(key, typedValue)
	}

	if err != nil {
		return fmt.Errorf("failed to set config: %w", err)
	}

	target := "global"
	if configSetLocal {
		target = "local (.kwt.toml)"
	}
	fmt.Printf("Set %s = %v (%s)\n", key, typedValue, target)

	if key == "cd.launch_shell" {
		w := cmd.ErrOrStderr()
		_, _ = fmt.Fprintln(w, "\nTo apply this change, reload your shell integration:")
		_, _ = fmt.Fprintln(w, "  source <(kwt completion bash)   # bash")
		_, _ = fmt.Fprintln(w, "  source <(kwt completion zsh)    # zsh")
		_, _ = fmt.Fprintln(w, "  kwt completion fish | source    # fish")
		_, _ = fmt.Fprintln(w, "Or: exec $SHELL")
	}

	return nil
}

func runConfigGet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := config.GetValue(key)

	if value == nil {
		return fmt.Errorf("configuration key '%s' not found - use 'kwt config list' to see available keys", key)
	}

	fmt.Println(value)
	return nil
}
