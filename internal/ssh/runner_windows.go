//go:build windows

package ssh

import "context"

func runSSHProcess(
	ctx context.Context,
	arguments []string,
	workingDirectory string,
	environment []string,
	run OutputRunner,
) (int, error) {
	_, stderr, exitCode, err := run(
		ctx,
		append([]string{"ssh"}, arguments...),
		workingDirectory,
		environment,
		nil,
	)
	return exitCode, sshProcessError(stderr, exitCode, err)
}
