package ssh

import (
	"context"
	"errors"
	"os"

	"go.kenn.io/kit/openssh"
	"go.kenn.io/kit/safefileio"
)

type sshProcessRunner func(
	context.Context,
	[]string,
	string,
	[]string,
) (int, error)

func NewRunner(
	privateDirectory string,
	request LeaseRequest,
	target ResolvedTarget,
) (openssh.RunSSH, error) {
	return newRunner(privateDirectory, request, target, runSSHProcess)
}

func newRunner(
	privateDirectory string,
	request LeaseRequest,
	target ResolvedTarget,
	run sshProcessRunner,
) (openssh.RunSSH, error) {
	projection := target.Projection
	environment := append([]string(nil), request.Environment...)
	return func(ctx context.Context, managerArguments []string) (int, error) {
		arguments, cleanup, err := materializeProjection(privateDirectory, projection)
		if err != nil {
			return -1, err
		}
		arguments = append(arguments, managerArguments...)
		exitCode, runErr := run(
			ctx,
			arguments,
			request.WorkingDirectory,
			environment,
		)
		return exitCode, errors.Join(runErr, cleanup())
	}, nil
}

func materializeProjection(
	privateDirectory string,
	projection ExecutionProjection,
) ([]string, func() error, error) {
	arguments := append([]string(nil), projection.Arguments...)
	if len(projection.PrivateConfig) == 0 {
		return arguments, func() error { return nil }, nil
	}
	if err := safefileio.EnsurePrivateDir(privateDirectory); err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(privateDirectory, "config-")
	if err != nil {
		return nil, nil, err
	}
	path := file.Name()
	cleanup := func() error {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, line := range projection.PrivateConfig {
		if _, err := file.WriteString(line + "\n"); err != nil {
			_ = file.Close()
			_ = cleanup()
			return nil, nil, err
		}
	}
	if err := file.Close(); err != nil {
		_ = cleanup()
		return nil, nil, err
	}
	return replaceConfigPath(arguments, path), cleanup, nil
}

func replaceConfigPath(arguments []string, path string) []string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == "-F" {
			arguments[index+1] = path
			return arguments
		}
	}
	return append([]string{"-F", path}, arguments...)
}
