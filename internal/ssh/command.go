package ssh

import (
	"context"
	"io"
	"os/exec"
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
) (int, error) {
	resolved, err := resolveExecutable(executable, environment, workingDirectory)
	if err != nil {
		return -1, err
	}
	command := exec.CommandContext(ctx, resolved, arguments...)
	command.Dir = workingDirectory
	command.Env = environment
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	err = runResolverCommand(command)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), err
	}
	return -1, err
}
