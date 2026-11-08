// Package herdrcore manages kwt's local Herdr Core terminals through hc.
package herdrcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"go.kenn.io/kwt/internal/credentials"
)

const managedLabel = "kwt.managed=true"

var errUnmanaged = errors.New("not a local kwt-managed terminal")

// Terminal is the part of hc's full terminal record used by kwt. Addresses are
// immutable Core identities; names and labels are only selection aids.
type Terminal struct {
	Address   string            `json:"address"`
	Name      string            `json:"name,omitempty"`
	Labels    map[string]string `json:"labels"`
	Argv      []string          `json:"argv,omitempty"`
	Directory string            `json:"cwd"`
	State     string            `json:"state"`
	Keep      bool              `json:"keep"`
	Reason    string            `json:"reason"`
	CreatedMS int64             `json:"created_ms"`
	ExitCode  *int              `json:"exit_code,omitempty"`
	Status    *Status           `json:"status,omitempty"`
}

// Status is the program's own reported state, when Core has one.
type Status struct {
	App   string `json:"app,omitempty"`
	State string `json:"state"`
}

func (t Terminal) Identifier() string { return t.Labels["kwt.identifier"] }
func (t Terminal) Context() string    { return t.Labels["kwt.context"] }

// RunOptions describes a command, with kwt's executable providing the
// pre-shell credential boundary inside the Core-inherited environment.
type RunOptions struct {
	Executable     string
	Directory      string
	Identifier     string
	Context        string
	Command        string
	ProtectedNames []string
	AutoCleanup    bool
}

// Client keeps process execution at one boundary for tests. It never retries
// mutations: a disconnected launch or kill may already have taken effect.
type Client struct {
	workspaceMu    sync.Mutex
	socket         string
	pinned         bool
	protectedNames []string
	run            func(context.Context, ...string) ([]byte, error)
	runLaunch      func(context.Context, []string, ...string) ([]byte, error)
	attach         func(context.Context, string) error
}

func NewClient() *Client {
	return &Client{run: runHC, runLaunch: func(ctx context.Context, names []string, args ...string) ([]byte, error) {
		return runHCEnvironment(ctx, launchEnvironment(names), args...)
	}, attach: attachHC}
}

func launchEnvironment(names []string) []string {
	return credentials.StripEnvironment(os.Environ(), append(credentials.ProtectedNames(nil), names...))
}

type commandError struct {
	kind string
	err  error
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

func runHC(ctx context.Context, args ...string) ([]byte, error) {
	return runHCEnvironment(ctx, credentials.StripEnvironment(os.Environ(), credentials.ProtectedNames(nil)), args...)
}

func runHCEnvironment(ctx context.Context, env []string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "hc", args...)
	// A newly started Core must not capture kwt credentials from its launcher.
	// The helper also removes configured credentials from an existing Core's env.
	command.Env = env
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		var failure struct {
			Error struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(stderr.Bytes(), &failure) == nil && failure.Error.Kind != "" {
			return nil, &commandError{kind: failure.Error.Kind, err: fmt.Errorf("hc: %s: %s: %w", failure.Error.Kind, failure.Error.Message, err)}
		}
		return nil, fmt.Errorf("hc: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func attachHC(ctx context.Context, address string) error {
	command := exec.CommandContext(ctx, "hc", "attach", "--", address)
	command.Env = credentials.StripEnvironment(os.Environ(), credentials.ProtectedNames(nil))
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func (c *Client) Run(ctx context.Context, opts RunOptions) (*Terminal, error) {
	if opts.Executable == "" || opts.Directory == "" || strings.TrimSpace(opts.Command) == "" {
		return nil, fmt.Errorf("kwt executable, working directory and command are required")
	}
	args := []string{"run", "--json", "--bg", "--outlive-parent", "--no-notify", "--cwd", opts.Directory,
		"--label", managedLabel, "--label", "kwt.identifier=" + opts.Identifier, "--label", "kwt.context=" + opts.Context}
	if opts.AutoCleanup {
		args = append(args, "--rm")
	}
	args = append(args, "--", opts.Executable, "herdr", "_run-command")
	for _, name := range opts.ProtectedNames {
		args = append(args, "--strip", name)
	}
	args = append(args, "--", opts.Command)
	var output []byte
	var err error
	if c.runLaunch != nil {
		output, err = c.runLaunch(ctx, opts.ProtectedNames, args...)
	} else {
		output, err = c.run(ctx, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("Core launch failed; inspect 'kwt herdr list --all' before retrying: %w", err)
	}
	var terminal Terminal
	if err := json.Unmarshal(output, &terminal); err != nil || !localAddress(terminal.Address) {
		return nil, fmt.Errorf("Core returned an invalid launch record; inspect 'kwt herdr list --all' before retrying")
	}
	return &terminal, nil
}

func localAddress(address string) bool {
	return address != "" && !strings.Contains(address, "/") && !strings.HasPrefix(address, "-")
}

func (c *Client) getManaged(ctx context.Context, address string) (*Terminal, error) {
	if !localAddress(address) {
		return nil, fmt.Errorf("a local Core terminal address is required")
	}
	output, err := c.run(ctx, "get", "--json", "--", address)
	if err != nil {
		return nil, err
	}
	var terminal Terminal
	if err := json.Unmarshal(output, &terminal); err != nil {
		return nil, fmt.Errorf("decode Core terminal: %w", err)
	}
	if terminal.Address != address || terminal.Labels["kwt.managed"] != "true" {
		return nil, fmt.Errorf("terminal %q: %w", address, errUnmanaged)
	}
	return &terminal, nil
}

func (c *Client) List(ctx context.Context, all bool) ([]Terminal, error) {
	args := []string{"ls", "--json", "--label", managedLabel}
	if all {
		args = append(args, "--all")
	}
	output, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	// hc ls uses a compact CLI view, unlike terminal.list's full API records.
	// Fetch each record to obtain labels and to recheck ownership.
	var list struct {
		Terminals []struct {
			Address string `json:"address"`
		} `json:"terminals"`
	}
	if err := json.Unmarshal(output, &list); err != nil {
		return nil, fmt.Errorf("decode Core terminal list: %w", err)
	}
	terminals := make([]Terminal, 0, len(list.Terminals))
	for _, item := range list.Terminals {
		if !localAddress(item.Address) {
			continue
		}
		record, err := c.getManaged(ctx, item.Address)
		if err != nil {
			var failure *commandError
			if errors.As(err, &failure) && failure.kind == "not_found" {
				continue
			} // exited --rm terminal
			// A label can change between ls and get; never adopt an unowned terminal.
			if errors.Is(err, errUnmanaged) {
				continue
			}
			return nil, err
		}
		if !all && record.State == "finished" {
			continue
		}
		terminals = append(terminals, *record)
	}
	return terminals, nil
}

// Resolve deliberately refuses ambiguous patterns. Exact immutable addresses
// win; a label or name must identify exactly one terminal before any mutation.
func Resolve(terminals []Terminal, pattern string) (*Terminal, error) {
	if pattern == "" {
		return nil, fmt.Errorf("a terminal address or pattern is required")
	}
	for i := range terminals {
		if terminals[i].Address == pattern {
			return &terminals[i], nil
		}
	}
	var matches []*Terminal
	pattern = strings.ToLower(pattern)
	for i := range terminals {
		terminal := &terminals[i]
		if strings.Contains(strings.ToLower(terminal.Identifier()), pattern) || strings.Contains(strings.ToLower(terminal.Context()), pattern) || strings.Contains(strings.ToLower(terminal.Name), pattern) {
			matches = append(matches, terminal)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no Core terminal matches %q", pattern)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("multiple Core terminals match %q; use an exact address from 'kwt herdr list'", pattern)
	}
}

func (c *Client) Attach(ctx context.Context, address string) error {
	terminal, err := c.getManaged(ctx, address)
	if err != nil {
		return err
	}
	if terminal.State != "live" {
		return fmt.Errorf("terminal is finished; inspect its output with 'hc read %s'", address)
	}
	return c.attach(ctx, address)
}

func (c *Client) Kill(ctx context.Context, address string) error {
	terminal, err := c.getManaged(ctx, address)
	if err != nil {
		return err
	}
	if terminal.State != "finished" {
		if _, err := c.run(ctx, "kill", "--json", "--", address); err != nil {
			return fmt.Errorf("kill Core terminal %s (inspect before retrying): %w", address, err)
		}
	}
	if _, err := c.run(ctx, "rm", "--json", "--", address); err != nil {
		var failure *commandError
		if errors.As(err, &failure) && failure.kind == "not_found" {
			return nil
		} // Core --rm already removed it
		return fmt.Errorf("remove Core terminal %s (inspect before retrying): %w", address, err)
	}
	return nil
}
