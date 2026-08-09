// Package git provides Git operations for the kwt application.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.kenn.io/kwt/internal/credentials"
)

// Git provides Git command operations.
type Git struct {
	workDir     string
	ctx         context.Context
	environment []string
}

// New creates a new Git instance.
func New(workDir string) *Git {
	return &Git{
		workDir: workDir,
	}
}

// NewWithContext creates a Git instance whose operations stop when ctx is canceled.
func NewWithContext(ctx context.Context, workDir string) *Git {
	return &Git{workDir: workDir, ctx: ctx}
}

// NewForInventory creates a Git instance isolated from repository-routing
// variables and credentials inherited by a long-lived daemon.
func NewForInventory(
	ctx context.Context,
	workDir string,
	protectedNames []string,
) *Git {
	return &Git{
		workDir: workDir,
		ctx:     ctx,
		environment: inventoryEnvironment(
			os.Environ(),
			protectedNames,
		),
	}
}

func inventoryEnvironment(env, protectedNames []string) []string {
	names := append(credentials.ProtectedNames(nil), protectedNames...)
	filtered := credentials.StripEnvironment(env, names)
	result := make([]string, 0, len(filtered)+1)
	for _, entry := range filtered {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "GIT_TERMINAL_PROMPT=0")
}

// NewFromCwd creates a new Git instance using the current working directory.
func NewFromCwd() (*Git, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current directory: %w", err)
	}
	return New(cwd), nil
}

// RunCommand executes a git command with the provided arguments and returns the output.
// The command is executed in the Git instance's working directory if set.
func (g *Git) RunCommand(args ...string) (string, error) {
	return g.run(args...)
}

// RunCommandWithoutCredentials executes a Git command after removing the
// named credentials from its environment. Use it for commands that may run
// hooks or helpers selected by untrusted worktree content or configuration.
func (g *Git) RunCommandWithoutCredentials(
	protectedNames []string,
	args ...string,
) (string, error) {
	return g.runWithoutCredentials(protectedNames, args...)
}

// RunWithContext executes a git command with context support for cancellation and timeout.
func (g *Git) RunWithContext(ctx context.Context, args ...string) (string, error) {
	return g.runWithContext(ctx, args...)
}

// run executes a git command.
func (g *Git) run(args ...string) (string, error) {
	cmd := g.command(args...)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if g.ctx != nil && g.ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), g.ctx.Err())
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), stderr.String())
	}

	return stdout.String(), nil
}

// runWithoutCredentials executes a Git operation that may run against
// untrusted content without exposing caller-supplied credentials.
func (g *Git) runWithoutCredentials(
	protectedNames []string,
	args ...string,
) (string, error) {
	cmd := g.command(args...)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}
	environment := os.Environ()
	if g.environment != nil {
		environment = g.environment
	}
	cmd.Env = credentials.StripEnvironment(environment, protectedNames)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if g.ctx != nil && g.ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), g.ctx.Err())
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), stderr.String())
	}
	return stdout.String(), nil
}

func (g *Git) command(args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if g.ctx != nil {
		cmd = exec.CommandContext(g.ctx, "git", args...)
	} else {
		cmd = exec.Command("git", args...)
	}
	if g.environment != nil {
		cmd.Env = append([]string(nil), g.environment...)
	}
	return cmd
}

// runWithContext executes a git command with context support.
func (g *Git) runWithContext(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if g.environment != nil {
		cmd.Env = append([]string(nil), g.environment...)
	}
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), stderr.String())
	}

	return stdout.String(), nil
}
