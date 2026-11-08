package herdrcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kwt/internal/credentials"
)

// NewWorkspaceClient pins every operation to the machine's configured Core.
// Standalone commands continue to use NewClient and the ambient hc route.
func NewWorkspaceClient(socket string, names ...string) *Client {
	c := &Client{socket: socket, pinned: true, protectedNames: append([]string(nil), names...)}
	c.run = func(ctx context.Context, args ...string) ([]byte, error) {
		if !filepath.IsAbs(c.socket) {
			return nil, fmt.Errorf("workspace.core_socket must be absolute")
		}
		return runHCEnvironment(ctx, c.Environment(), args...)
	}
	c.attach = func(ctx context.Context, address string) error { return c.AttachCommand(ctx, address).Run() }
	return c
}

func (c *Client) Environment() []string {
	env := credentials.StripEnvironment(os.Environ(), append(credentials.ProtectedNames(nil), c.protectedNames...))
	if c.socket == "" {
		return env
	}
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		k, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(k, "HC_SOCKET") || strings.EqualFold(k, "HC_SESSION") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "HC_SOCKET="+c.socket, "HC_SESSION=kwt")
}

// AttachCommand prepares a resident child without starting it. The caller must
// freshly verify the endpoint while holding its existing worktree guard.
func (c *Client) AttachCommand(ctx context.Context, address string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "hc", "attach", "--", address)
	cmd.Env = c.Environment()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

func workspaceNames(opts RunOptions) []string {
	names := append(credentials.ProtectedNames(nil), opts.ProtectedNames...)
	for i := range names {
		names[i] = strings.ToUpper(strings.TrimSpace(names[i]))
	}
	slices.Sort(names)
	return slices.Compact(slices.DeleteFunc(names, func(s string) bool { return s == "" }))
}
func workspaceArgv(opts RunOptions, generation, path string) []string {
	argv := []string{opts.Executable, "herdr", "_run-command", "--workspace", "--workspace-path", path, "--workspace-generation", generation}
	for _, name := range workspaceNames(opts) {
		argv = append(argv, "--strip", name)
	}
	return append(argv, "--", "")
}
func workspaceKey(opts RunOptions, generation string) string {
	if generation != "" {
		return "generation:" + generation
	}
	return "path:" + filepath.Clean(opts.Directory)
}
func workspaceName(opts RunOptions, generation string) string {
	hash := sha256.Sum256([]byte(workspaceKey(opts, generation)))
	return "kwt-ws-" + hex.EncodeToString(hash[:12])
}
func validateWorkspaceOptions(opts RunOptions) error {
	if opts.Executable == "" || !filepath.IsAbs(opts.Directory) {
		return fmt.Errorf("workspace executable and absolute directory required")
	}
	if opts.Context != "workspace" && opts.Context != "protected-workspace" {
		return fmt.Errorf("invalid Core workspace context")
	}
	if opts.Context == "protected-workspace" && opts.Command != "" {
		return fmt.Errorf("protected workspace requires a blank shell")
	}
	return nil
}

// VerifyWorkspace validates original execution authority, not mutable names.
// A durable generation lets an already guarded worktree move retain its shell.
func VerifyWorkspace(t Terminal, opts RunOptions, generation string) error {
	if !localAddress(t.Address) || t.CreatedMS <= 0 || t.Keep || t.Reason == "spawn_pending" || t.Labels["kwt.managed"] != "true" || t.Context() != opts.Context {
		return fmt.Errorf("Core workspace identity is uncertain or changed")
	}
	if t.Labels["kwt.workspace_key"] != workspaceKey(opts, generation) {
		return fmt.Errorf("Core workspace registration changed")
	}
	if !filepath.IsAbs(t.Directory) || !slices.Equal(t.Argv, workspaceArgv(opts, generation, t.Directory)) {
		return fmt.Errorf("Core workspace helper or credential policy changed")
	}
	if t.Labels["kwt.workspace_path"] != t.Directory || t.Labels["kwt.generation"] != generation {
		return fmt.Errorf("Core workspace path or generation changed")
	}
	if generation == "" && filepath.Clean(t.Directory) != filepath.Clean(opts.Directory) {
		return fmt.Errorf("Core workspace directory changed")
	}
	if t.Labels["kwt.initialized"] != "true" {
		return fmt.Errorf("Core workspace initialization is uncertain; inspect it before retrying")
	}
	if t.State != "live" && t.State != "finished" {
		return fmt.Errorf("unknown Core workspace state %q", t.State)
	}
	return nil
}

func (c *Client) resolveWorkspace(ctx context.Context, opts RunOptions, generation string) (*Terminal, error) {
	if err := validateWorkspaceOptions(opts); err != nil {
		return nil, err
	}
	terminals, err := c.List(ctx, true)
	if err != nil {
		return nil, err
	}
	var selected *Terminal
	for _, t := range terminals {
		if t.Labels["kwt.workspace_key"] != workspaceKey(opts, generation) && t.Name != workspaceName(opts, generation) {
			continue
		}
		if t.State == "finished" && t.Reason != "spawn_pending" && t.Labels["kwt.initialized"] == "true" {
			continue
		}
		if err := VerifyWorkspace(t, opts, generation); err != nil {
			return nil, err
		}
		if selected != nil {
			return nil, fmt.Errorf("multiple live Core workspaces; inspect exact addresses")
		}
		copy := t
		selected = &copy
	}
	if selected == nil {
		return nil, nil
	}
	fresh, err := c.getManaged(ctx, selected.Address)
	if err != nil {
		return nil, err
	}
	if fresh.CreatedMS != selected.CreatedMS {
		return nil, fmt.Errorf("Core workspace creation identity changed")
	}
	if err := VerifyWorkspace(*fresh, opts, generation); err != nil {
		return nil, err
	}
	if fresh.State != "live" {
		return nil, fmt.Errorf("Core workspace exited during selection; open again explicitly")
	}
	return fresh, nil
}

// ResolveWorkspace only observes; dashboard refresh never creates terminals.
func (c *Client) ResolveWorkspace(ctx context.Context, opts RunOptions, generation string) (*Terminal, error) {
	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()
	return c.resolveWorkspace(ctx, opts, generation)
}

// ObserveWorkspace never invokes hc, including when the configured Core is absent.
func (c *Client) ObserveWorkspace(ctx context.Context, opts RunOptions, generation string) (*Terminal, error) {
	if err := validateWorkspaceOptions(opts); err != nil {
		return nil, err
	}
	terminals, err := c.ObserveManaged(ctx)
	if err != nil {
		return nil, err
	}
	return SelectObservedWorkspace(terminals, opts, generation)
}

// SelectObservedWorkspace resolves one request from a previously verified inventory.
func SelectObservedWorkspace(terminals []Terminal, opts RunOptions, generation string) (*Terminal, error) {
	if err := validateWorkspaceOptions(opts); err != nil {
		return nil, err
	}
	var selected *Terminal
	for _, t := range terminals {
		if t.Labels["kwt.workspace_key"] != workspaceKey(opts, generation) && t.Name != workspaceName(opts, generation) {
			continue
		}
		if t.State == "finished" && t.Reason != "spawn_pending" && t.Labels["kwt.initialized"] == "true" {
			continue
		}
		if err := VerifyWorkspace(t, opts, generation); err != nil {
			return nil, err
		}
		if selected != nil {
			return nil, fmt.Errorf("multiple live Core workspaces; inspect exact addresses")
		}
		copy := t
		selected = &copy
	}
	return selected, nil
}

func (c *Client) EstablishWorkspace(ctx context.Context, opts RunOptions, generation string) (*Terminal, error) {
	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()
	terminal, err := c.resolveWorkspace(ctx, opts, generation)
	if err != nil || terminal != nil {
		return terminal, err
	}
	initialized := "true"
	if opts.Command != "" {
		initialized = "false"
	}
	args := []string{"run", "--json", "--bg", "--no-parent", "--no-notify", "--name", workspaceName(opts, generation), "--cwd", opts.Directory}
	for _, label := range []string{managedLabel, "kwt.identifier=" + opts.Identifier, "kwt.context=" + opts.Context, "kwt.workspace_key=" + workspaceKey(opts, generation), "kwt.workspace_path=" + opts.Directory, "kwt.generation=" + generation, "kwt.initialized=" + initialized} {
		args = append(args, "--label", label)
	}
	args = append(args, "--")
	args = append(args, workspaceArgv(opts, generation, opts.Directory)...)
	output, err := c.run(ctx, args...)
	if err != nil {
		var failure *commandError
		if errors.As(err, &failure) && failure.kind == "name_taken" {
			existing, resolveErr := c.resolveWorkspace(ctx, opts, generation)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if existing == nil {
				return nil, fmt.Errorf("Core workspace name conflict without a verified live workspace; inspect before retrying")
			}
			return existing, nil
		}
		return nil, fmt.Errorf("Core workspace launch uncertain; inspect before retrying: %w", err)
	}
	var compact Terminal
	if json.Unmarshal(output, &compact) != nil || !localAddress(compact.Address) {
		return nil, fmt.Errorf("invalid Core launch result; inspect before retrying")
	}
	terminal, err = c.getManaged(ctx, compact.Address)
	if err != nil {
		return nil, err
	}
	// Pending initialization is permitted only for this exact fresh launch.
	check := *terminal
	check.Labels = make(map[string]string, len(terminal.Labels))
	for k, v := range terminal.Labels {
		check.Labels[k] = v
	}
	check.Labels["kwt.initialized"] = "true"
	if err := VerifyWorkspace(check, opts, generation); err != nil {
		return nil, err
	}
	if terminal.State != "live" {
		return nil, fmt.Errorf("Core workspace did not start; inspect before retrying")
	}
	if opts.Command != "" {
		if _, err = c.run(ctx, "send", "--", terminal.Address, opts.Command); err != nil {
			return nil, fmt.Errorf("Core initial command delivery uncertain; never resubmit automatically: %w", err)
		}
		if _, err = c.run(ctx, "label", terminal.Address, "kwt.initialized=true"); err != nil {
			return nil, fmt.Errorf("Core initialization acknowledgement uncertain: %w", err)
		}
		terminal.Labels["kwt.initialized"] = "true"
	}
	return terminal, nil
}

func (c *Client) CurrentWorkspace(ctx context.Context, opts RunOptions, generation string, expected Terminal) (*Terminal, error) {
	fresh, err := c.getManaged(ctx, expected.Address)
	if err != nil {
		return nil, err
	}
	if fresh.CreatedMS != expected.CreatedMS {
		return nil, fmt.Errorf("Core workspace creation identity changed")
	}
	if err := VerifyWorkspace(*fresh, opts, generation); err != nil {
		return nil, err
	}
	return fresh, nil
}
func (c *Client) StopWorkspace(ctx context.Context, opts RunOptions, generation string, expected Terminal) error {
	fresh, err := c.CurrentWorkspace(ctx, opts, generation, expected)
	if err != nil {
		return err
	}
	if fresh.State == "finished" {
		return nil
	}
	_, err = c.run(ctx, "kill", "--json", "--", fresh.Address)
	return err
}
func (c *Client) RemoveFinishedWorkspace(ctx context.Context, opts RunOptions, generation string, expected Terminal) error {
	fresh, err := c.CurrentWorkspace(ctx, opts, generation, expected)
	if err != nil {
		return err
	}
	if fresh.State != "finished" {
		return fmt.Errorf("refusing to remove a live Core workspace")
	}
	_, err = c.run(ctx, "rm", "--json", "--", fresh.Address)
	return err
}
