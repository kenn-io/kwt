// Package git provides Git operations for the kwt application.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kwt/internal/credentials"
)

const (
	commandOutputLimit = 1 << 20
	commandWaitDelay   = 100 * time.Millisecond
)

var (
	ErrStdoutLimitExceeded = errors.New("git stdout limit exceeded")
	ErrStderrLimitExceeded = errors.New("git stderr limit exceeded")
)

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		_, _ = b.buffer.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.exceeded = true
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}

func (b *boundedBuffer) String() string {
	return b.buffer.String()
}

// Git provides Git command operations.
type Git struct {
	workDir     string
	ctx         context.Context
	environment []string
	executable  string
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
		if ok && isInventoryGitRedirect(name) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "GIT_TERMINAL_PROMPT=0")
}

func isInventoryGitRedirect(name string) bool {
	switch strings.ToUpper(name) {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_NAMESPACE":
		return true
	default:
		return false
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

// RunBytesWithEnvironment executes a Git command with deterministic
// environment overrides and returns stdout without text conversion.
func (g *Git) RunBytesWithEnvironment(
	overrides map[string]string,
	args ...string,
) ([]byte, error) {
	cmd := g.command(args...)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}
	environment := os.Environ()
	if g.environment != nil {
		environment = g.environment
	}
	cmd.Env = environmentWithOverrides(environment, overrides)

	stdout := boundedBuffer{limit: commandOutputLimit}
	stderr := boundedBuffer{limit: commandOutputLimit}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	var limitErrors []error
	if stdout.exceeded {
		limitErrors = append(limitErrors, ErrStdoutLimitExceeded)
	}
	if stderr.exceeded {
		limitErrors = append(limitErrors, ErrStderrLimitExceeded)
	}
	if len(limitErrors) > 0 {
		if runErr != nil {
			limitErrors = append(limitErrors, runErr)
		}
		return nil, fmt.Errorf(
			"git %s: %w",
			strings.Join(args, " "),
			errors.Join(limitErrors...),
		)
	}
	if runErr != nil {
		if g.ctx != nil && g.ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), g.ctx.Err())
		}
		if stderr.String() == "" {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), runErr)
		}
		return nil, fmt.Errorf(
			"git %s: %w: %s",
			strings.Join(args, " "),
			runErr,
			stderr.String(),
		)
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func environmentWithOverrides(
	environment []string,
	overrides map[string]string,
) []string {
	names := make(map[string]bool, len(overrides))
	keys := make([]string, 0, len(overrides))
	for name := range overrides {
		names[strings.ToLower(name)] = true
		keys = append(keys, name)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if ok && names[strings.ToLower(name)] {
			continue
		}
		result = append(result, entry)
	}
	for _, name := range keys {
		result = append(result, name+"="+overrides[name])
	}
	return result
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

	if err := cmd.Run(); err != nil && !successfulAfterWaitDelay(cmd, err) {
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
	if err := cmd.Run(); err != nil && !successfulAfterWaitDelay(cmd, err) {
		if g.ctx != nil && g.ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), g.ctx.Err())
		}
		if stderr.String() == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf(
			"git %s: %w: %s",
			strings.Join(args, " "),
			err,
			stderr.String(),
		)
	}
	return stdout.String(), nil
}

func (g *Git) command(args ...string) *exec.Cmd {
	executable := g.executable
	if executable == "" {
		executable = "git"
	}
	var cmd *exec.Cmd
	if g.ctx != nil {
		cmd = exec.CommandContext(g.ctx, executable, args...)
	} else {
		cmd = exec.Command(executable, args...)
	}
	if g.environment != nil {
		cmd.Env = append([]string(nil), g.environment...)
	}
	cmd.WaitDelay = commandWaitDelay
	return cmd
}

func successfulAfterWaitDelay(cmd *exec.Cmd, err error) bool {
	return errors.Is(err, exec.ErrWaitDelay) &&
		cmd.ProcessState != nil &&
		cmd.ProcessState.Success()
}

// runWithContext executes a git command with context support.
func (g *Git) runWithContext(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = commandWaitDelay
	if g.environment != nil {
		cmd.Env = append([]string(nil), g.environment...)
	}
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil && !successfulAfterWaitDelay(cmd, err) {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), stderr.String())
	}

	return stdout.String(), nil
}
