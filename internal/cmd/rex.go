package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/rex"
	"golang.org/x/term"
)

var rexCmd = &cobra.Command{
	Use:   "rex",
	Short: "List and attach to terminals on a Rex server",
	Long: `List and attach to the terminals on this account's Rex server.

Rex owns its sessions, windows, and layout. These commands show one Rex
terminal at a time in an ordinary terminal; detaching leaves it running.
They never start a Rex server.`,
	Example: `  # List Rex terminals as JSON
  kwt rex ls --json

  # Attach to one terminal
  kwt rex attach block:01a1...`,
}

var rexListJSON bool

var rexListCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List the terminals in every Rex session",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		listing, err := rexListing(commandContext(cmd), rex.Executables())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if rexListJSON {
			encoder := json.NewEncoder(out)
			encoder.SetIndent("", "  ")
			return encoder.Encode(listing)
		}
		switch listing.Status {
		case rexNotInstalled:
			_, err = fmt.Fprintln(out, "Rex is not installed.")
		case rexNotRunning:
			_, err = fmt.Fprintln(out, "The Rex server is not running.")
		default:
			for _, t := range listing.Terminals {
				name := t.Label
				if t.Title != "" {
					name = t.Title
				}
				if _, err = fmt.Fprintf(out, "%s\t%s\t%s\t%s %s\n",
					t.SessionLabel, t.WindowLabel, name, t.SessionID, t.BlockID); err != nil {
					return err
				}
			}
		}
		return err
	},
}

var rexAttachCmd = &cobra.Command{
	Use:   "attach BLOCK_ID",
	Short: "Attach this terminal to one Rex terminal",
	Long: `Show one Rex terminal here and type into it.

The attach takes the terminal's size from this one. Close this terminal to
detach; the program in the Rex terminal keeps running. The attach ends by
itself when that program exits.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := commandContext(cmd)
		server, err := rex.FindServer(ctx, rex.Executables())
		if err != nil {
			return err
		}
		client, err := rex.Dial(ctx, server.Socket)
		if err != nil {
			return err
		}
		terminal, err := client.Terminal(ctx, args[0])
		_ = client.Close()
		if err != nil {
			return err
		}
		return runRexAttach(ctx, server.Socket, terminal.SessionID, terminal.BlockID)
	},
}

func init() {
	rootCmd.AddCommand(rexCmd)
	rexCmd.AddCommand(rexListCmd, rexAttachCmd)
	rexListCmd.Flags().BoolVar(&rexListJSON, "json", false, "Output as JSON")
}

const (
	rexRunning      = "running"
	rexNotInstalled = "not_installed"
	rexNotRunning   = "not_running"
)

// rexListingOutput is the `kwt rex ls --json` contract. A missing or stopped
// Rex is a normal state, reported in status with no terminals.
type rexListingOutput struct {
	Status    string         `json:"status"`
	Server    *rex.Server    `json:"server,omitempty"`
	Terminals []rex.Terminal `json:"terminals"`
}

func rexListing(ctx context.Context, executables []string) (rexListingOutput, error) {
	listing := rexListingOutput{Terminals: []rex.Terminal{}}
	server, err := rex.FindServer(ctx, executables)
	switch {
	case errors.Is(err, rex.ErrNotInstalled):
		listing.Status = rexNotInstalled
		return listing, nil
	case errors.Is(err, rex.ErrNotRunning):
		listing.Status = rexNotRunning
		return listing, nil
	case err != nil:
		return listing, err
	}
	client, err := rex.Dial(ctx, server.Socket)
	if err != nil {
		return listing, err
	}
	defer client.Close()
	terminals, err := client.Terminals(ctx)
	if err != nil {
		return listing, err
	}
	listing.Status = rexRunning
	listing.Server = &server
	listing.Terminals = terminals
	return listing, nil
}

func runRexAttach(ctx context.Context, socket, sessionID, blockID string) error {
	stdin := int(os.Stdin.Fd()) //nolint:gosec // File descriptors fit in int.
	if !term.IsTerminal(stdin) {
		return errors.New("kwt rex attach needs a terminal")
	}
	columns, rows, err := term.GetSize(stdin)
	if err != nil {
		return fmt.Errorf("read the terminal size: %w", err)
	}
	state, err := term.MakeRaw(stdin)
	if err != nil {
		return fmt.Errorf("put the terminal in raw mode: %w", err)
	}
	defer func() { _ = term.Restore(stdin, state) }()

	sizes, stop := watchTerminalSize(ctx, stdin)
	defer stop()
	_, err = rex.Attach(ctx, socket, rex.AttachOptions{
		SessionID: sessionID,
		BlockID:   blockID,
		In:        os.Stdin,
		Out:       os.Stdout,
		Size:      rex.Size{Columns: columns, Rows: rows},
		Sizes:     sizes,
	})
	if err != nil {
		// Leave the error on its own line below the terminal's contents.
		_, _ = io.WriteString(os.Stdout, "\r\n")
	}
	return err
}
