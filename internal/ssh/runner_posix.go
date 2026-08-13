//go:build !windows

package ssh

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/kwt/internal/credentials"
)

const sshCommandEnvironment = "KWT_SSH_EXEC_COMMAND"

func runSSHProcess(
	ctx context.Context,
	arguments []string,
	workingDirectory string,
	environment []string,
) (int, error) {
	return runSSHProcessWith(
		ctx,
		arguments,
		workingDirectory,
		environment,
		runOutput,
		accountLoginShell,
	)
}

func runSSHProcessWith(
	ctx context.Context,
	arguments []string,
	workingDirectory string,
	environment []string,
	run OutputRunner,
	loginShell func(context.Context) (string, error),
) (int, error) {
	executable, err := resolveExecutable("ssh", environment, workingDirectory)
	if err != nil {
		return -1, err
	}
	shell, err := loginShell(ctx)
	if err != nil {
		return -1, err
	}
	if shell == "" {
		return -1, errors.New("account login shell is empty")
	}
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = shellQuote(argument)
	}
	command := "unset " + sshCommandEnvironment + "\n" +
		"cd " + shellQuote(workingDirectory) + " || exit $?\n" +
		"exec " + shellQuote(executable) + " " + strings.Join(quoted, " ")
	shellArguments, standardInput := shellCommandInvocation(shell, sshCommandEnvironment)
	shellEnvironment := credentials.StripEnvironment(environment, []string{sshCommandEnvironment})
	shellEnvironment = append(shellEnvironment, sshCommandEnvironment+"="+command)
	_, _, exitCode, err := run(
		ctx,
		shellArguments,
		workingDirectory,
		shellEnvironment,
		standardInput,
	)
	return exitCode, err
}
