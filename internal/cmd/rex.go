package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

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
	// Rex commands never use repository configuration, so they skip the
	// project merge and its trust prompt.
	PersistentPreRunE: globalOnlyPreRun,
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
				if _, err = fmt.Fprintf(out, "%s\t%s\t%s\t%s %s\n",
					t.SessionLabel, t.WindowLabel, t.Name, t.SessionID, t.BlockID); err != nil {
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
	// A SIGINT or SIGTERM from outside ends the attach through its context,
	// so the terminal is restored. Ctrl-C typed here is input for Rex.
	RunE: withGracefulSignals(func(cmd *cobra.Command, args []string) error {
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
	}),
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
	if serverGone(err) {
		listing.Status = rexNotRunning
		return listing, nil
	}
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

// serverGone reports a Rex socket that is missing or refuses connections:
// REX_SERVER names a stopped server, or the server stopped after discovery.
func serverGone(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// resetTerminalModes undoes modes that the painted screen or the program
// may have turned on and that would outlive an attach that ends abruptly:
// the alternate screen, a hidden cursor, mouse and focus reporting,
// bracketed paste, extended keyboard modes, application keys, and styles.
// It does not move the cursor.
const resetTerminalModes = "\x1b[?1047l\x1b[?25h" +
	"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l" +
	"\x1b[?1004l\x1b[?2004l\x1b[=0u\x1b[>4;0m\x1b[?1l\x1b>\x1b[0m"

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
	defer func() {
		_, _ = io.WriteString(os.Stdout, resetTerminalModes)
		_ = term.Restore(stdin, state)
	}()

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
