package ssh

import (
	"context"
	"errors"
	"io"
)

// RunClientProcess runs an OpenSSH-family client with request-scoped process
// state. Cancellation terminates the complete process tree, including proxy
// commands started by OpenSSH.
func RunClientProcess(
	ctx context.Context,
	executable string,
	arguments []string,
	workingDirectory string,
	environment []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) (int, bool, error) {
	resolved, err := resolveExecutable(executable, environment, workingDirectory)
	if err != nil {
		return -1, false, err
	}
	command := newClientCommand(ctx, resolved, arguments...)
	command.Dir = workingDirectory
	command.Env = environment
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	started, err := runClientCommand(ctx, command)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, started, ctxErr
	}
	if err == nil {
		return 0, started, nil
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), started, err
	}
	return -1, started, err
}
