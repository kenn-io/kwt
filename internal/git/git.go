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
	workDir string
}

// New creates a new Git instance.
func New(workDir string) *Git {
	return &Git{
		workDir: workDir,
	}
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

// RunWithContext executes a git command with context support for cancellation and timeout.
func (g *Git) RunWithContext(ctx context.Context, args ...string) (string, error) {
	return g.runWithContext(ctx, args...)
}

// run executes a git command.
func (g *Git) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
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
	cmd := exec.Command("git", args...)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}
	cmd.Env = credentials.StripEnvironment(os.Environ(), protectedNames)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), stderr.String())
	}
	return stdout.String(), nil
}

// runWithContext executes a git command with context support.
func (g *Git) runWithContext(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
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
