//go:build windows

package ssh

import "context"

func runSSHProcess(
	ctx context.Context,
	arguments []string,
	workingDirectory string,
	environment []string,
) (int, error) {
	_, _, exitCode, err := runOutput(
		ctx,
		append([]string{"ssh"}, arguments...),
		workingDirectory,
		environment,
		nil,
	)
	return exitCode, err
}
