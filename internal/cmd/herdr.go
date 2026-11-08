package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/herdrcore"
)

type herdrClient interface {
	Run(context.Context, herdrcore.RunOptions) (*herdrcore.Terminal, error)
	List(context.Context, bool) ([]herdrcore.Terminal, error)
	Attach(context.Context, string) error
	Kill(context.Context, string) error
}

func init() { rootCmd.AddCommand(newHerdrCommand(herdrcore.NewClient())) }

func newHerdrCommand(client herdrClient) *cobra.Command {
	parent := &cobra.Command{
		Use: "herdr", Short: "Manage Herdr Core terminals for long-running commands",
		Long: `Run, list, attach to, and stop local kwt-managed Herdr Core terminals.
Requires hc (Herdr Core), separate from the older herdr multiplexer.
Terminals outlive kwt. Finished commands retain Core's history by default.
Workspace commands use workspace.backend; tmux remains the default.`,
		Example: `  kwt herdr run -w feature/auth --id dev "npm run dev"
  kwt herdr list --all --json
  kwt herdr attach dev
  kwt herdr kill dev`,
	}
	parent.AddCommand(newHerdrRunCommand(client), newHerdrListCommand(client), newHerdrAttachCommand(client), newHerdrKillCommand(client), newHerdrHelperCommand())
	return parent
}

func newHerdrRunCommand(client herdrClient) *cobra.Command {
	var worktree, identifier, sessionContext string
	var attach, cleanup bool
	command := &cobra.Command{
		Use: "run [flags] [--] command", Short: "Run a shell command in a persistent Core terminal",
		Long: `Start a detached Core terminal in the current directory or a selected worktree.
Quote shell syntax as one command. Use --no-detach to attach immediately;
Ctrl+\ detaches. An ordinary exit retains a finished record and output, not a
replacement shell. On a launch error, inspect list --all before retrying.`,
		Args: cobra.MinimumNArgs(1),
		RunE: withGracefulSignals(func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			directory, err := os.Getwd()
			if err != nil {
				return err
			}
			if worktree != "" {
				directory, err = resolveWorktreePath(worktree, cfg)
				if err != nil {
					return err
				}
			}
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve kwt executable: %w", err)
			}
			line := strings.Join(args, " ")
			id := identifier
			if id == "" {
				id = generateIdentifierFromCommand(line, directory)
			}
			terminal, err := client.Run(commandContext(cmd), herdrcore.RunOptions{
				Executable: executable, Directory: directory, Identifier: id, Context: sessionContext, Command: line,
				ProtectedNames: credentials.ProtectedNames(cfg), AutoCleanup: cleanup,
			})
			if err != nil {
				return err
			}
			if attach {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created Core terminal: %s (%s)\nWorking directory: %s\n", terminal.Address, id, directory)
				return client.Attach(commandContext(cmd), terminal.Address)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created Core terminal: %s\nWorking directory: %s\n", id, directory)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Attach with 'kwt herdr attach %s' (Ctrl+\\ detaches).\n", terminal.Address)
			return nil
		}),
	}
	command.Flags().StringVarP(&worktree, "worktree", "w", "", "Run in a specific worktree")
	command.Flags().StringVar(&identifier, "id", "", "Custom identifier label")
	command.Flags().StringVar(&sessionContext, "context", "run", "Session context label")
	command.Flags().BoolVar(&attach, "no-detach", false, "Attach immediately after creation")
	command.Flags().BoolVar(&cleanup, "auto-cleanup", false, "Use Core --rm to remove the record after its exit is observed")
	return command
}

func newHerdrListCommand(client herdrClient) *cobra.Command {
	var all, asJSON, asCSV, watch bool
	command := &cobra.Command{
		Use: "list", Short: "List kwt-managed local Core terminals", Args: cobra.NoArgs,
		RunE: withGracefulSignals(func(cmd *cobra.Command, args []string) error {
			ctx := commandContext(cmd)
			refresh := func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				terminals, err := client.List(ctx, all)
				if err != nil {
					return err
				}
				if terminals == nil {
					terminals = []herdrcore.Terminal{}
				}
				switch {
				case asJSON:
					return json.NewEncoder(cmd.OutOrStdout()).Encode(terminals)
				case asCSV:
					writer := csv.NewWriter(cmd.OutOrStdout())
					if err := writer.Write([]string{"address", "identifier", "context", "state", "cwd"}); err != nil {
						return err
					}
					for _, terminal := range terminals {
						if err := writer.Write(herdrTerminalColumns(terminal)); err != nil {
							return err
						}
					}
					writer.Flush()
					return writer.Error()
				default:
					writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
					if _, err := fmt.Fprintln(writer, "ADDRESS\tIDENTIFIER\tCONTEXT\tSTATE\tDIRECTORY"); err != nil {
						return err
					}
					for _, terminal := range terminals {
						if _, err := fmt.Fprintln(writer, strings.Join(herdrTerminalColumns(terminal), "\t")); err != nil {
							return err
						}
					}
					return writer.Flush()
				}
			}
			if err := refresh(); err != nil {
				return err
			}
			if !watch {
				return nil
			}
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
					if err := refresh(); err != nil {
						return err
					}
				}
			}
		}),
	}
	command.Flags().BoolVar(&all, "all", false, "Include finished terminal records")
	command.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	command.Flags().BoolVar(&asCSV, "csv", false, "Output CSV")
	command.Flags().BoolVarP(&watch, "watch", "w", false, "Refresh every five seconds until canceled")
	command.MarkFlagsMutuallyExclusive("json", "csv")
	return command
}

func herdrTerminalColumns(terminal herdrcore.Terminal) []string {
	return []string{terminal.Address, terminal.Identifier(), terminal.Context(), terminal.State, terminal.Directory}
}

func newHerdrAttachCommand(client herdrClient) *cobra.Command {
	return &cobra.Command{
		Use: "attach <address-or-pattern>", Short: "Attach to one kwt-managed Core terminal (Ctrl+\\ detaches)", Args: cobra.ExactArgs(1),
		RunE: withGracefulSignals(func(cmd *cobra.Command, args []string) error {
			terminals, err := client.List(commandContext(cmd), false)
			if err != nil {
				return err
			}
			terminal, err := herdrcore.Resolve(terminals, args[0])
			if err != nil {
				return err
			}
			if terminal.Context() == "protected-workspace" {
				return fmt.Errorf("protected workspaces require kwt pr attach so current provenance and credentials are checked")
			}
			return client.Attach(commandContext(cmd), terminal.Address)
		}),
	}
}

func newHerdrKillCommand(client herdrClient) *cobra.Command {
	var all, force bool
	command := &cobra.Command{
		Use: "kill [address-or-pattern]", Short: "Stop kwt-managed Core terminals and remove their records", Args: cobra.MaximumNArgs(1),
		RunE: withGracefulSignals(func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return fmt.Errorf("choose a pattern or --all")
			}
			if !all && len(args) == 0 {
				return fmt.Errorf("a terminal address or pattern, or --all, is required")
			}
			terminals, err := client.List(commandContext(cmd), true)
			if err != nil {
				return err
			}
			if !all {
				terminal, err := herdrcore.Resolve(terminals, args[0])
				if err != nil {
					return err
				}
				terminals = []herdrcore.Terminal{*terminal}
			}
			if len(terminals) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No kwt-managed Core terminals found")
				return nil
			}
			if !force {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Stop and remove these Core terminals?")
				for _, terminal := range terminals {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s %s\n", terminal.Address, terminal.Identifier())
				}
				_, _ = fmt.Fprint(cmd.OutOrStdout(), "Continue? (y/N): ")
				var response string
				_, _ = fmt.Fscan(cmd.InOrStdin(), &response)
				if !strings.EqualFold(response, "y") && !strings.EqualFold(response, "yes") {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Operation cancelled")
					return nil
				}
			}
			for _, terminal := range terminals {
				if err := client.Kill(commandContext(cmd), terminal.Address); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed Core terminal: %s\n", terminal.Address)
			}
			return nil
		}),
	}
	command.Flags().BoolVar(&all, "all", false, "Select all kwt-managed local terminals, including finished records")
	command.Flags().BoolVar(&force, "force", false, "Skip confirmation")
	return command
}

// This helper is launched directly by Core, before any shell or repository
// command. Filtering hc's client environment alone cannot sanitize the daemon's
// already-captured environment. Never initialize kwt configuration here.
func newHerdrHelperCommand() *cobra.Command {
	var stripNames []string
	var workspace bool
	var workspacePath, workspaceGeneration string
	command := &cobra.Command{
		Use: "_run-command -- command", Hidden: true, Args: cobra.ExactArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			var shell *exec.Cmd
			if workspace {
				if args[0] != "" || workspacePath == "" {
					return fmt.Errorf("workspace helper requires a path and blank shell")
				}
				env := credentials.StripEnvironment(os.Environ(), append(credentials.ProtectedNames(nil), stripNames...))
				if runtime.GOOS == "windows" {
					shell = exec.CommandContext(commandContext(cmd), "cmd.exe", "/d")
				} else {
					path := "/bin/sh"
					for _, entry := range env {
						if strings.HasPrefix(entry, "SHELL=") && strings.TrimPrefix(entry, "SHELL=") != "" {
							path = strings.TrimPrefix(entry, "SHELL=")
						}
					}
					shell = exec.CommandContext(commandContext(cmd), path, "-l")
				}
				shell.Env = env
				shell.Dir = workspacePath
			} else {
				shell = herdrShellCommand(commandContext(cmd), args[0], stripNames)
			}
			shell.Stdin = os.Stdin
			shell.Stdout = os.Stdout
			shell.Stderr = os.Stderr
			return shell.Run()
		},
	}
	command.Flags().BoolVar(&workspace, "workspace", false, "Start a sanitized workspace login shell")
	command.Flags().StringVar(&workspacePath, "workspace-path", "", "Original workspace path")
	command.Flags().StringVar(&workspaceGeneration, "workspace-generation", "", "Worktree registration identity")
	command.Flags().StringArrayVar(&stripNames, "strip", nil, "Credential variable name to remove before invoking the shell")
	return command
}

func isHerdrRunHelper(command *cobra.Command) bool {
	return command.Name() == "_run-command" && command.Parent() != nil && command.Parent().Name() == "herdr"
}

func herdrShellCommand(ctx context.Context, line string, stripNames []string) *exec.Cmd {
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.CommandContext(ctx, "cmd.exe", "/d", "/s", "/c", line)
	} else {
		command = exec.CommandContext(ctx, "/bin/sh", "-c", line)
	}
	names := append(credentials.ProtectedNames(nil), stripNames...)
	command.Env = credentials.StripEnvironment(os.Environ(), names)
	return command
}
