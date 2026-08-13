package ssh

import (
	"context"
	"errors"
	"os"

	"go.kenn.io/kit/openssh"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kwt/internal/credentials"
)

type sshProcessRunner func(
	context.Context,
	[]string,
	string,
	[]string,
) (int, error)

type runnerOptions struct {
	Run        sshProcessRunner
	Version    InteractiveVersionPolicy
	Executable string
}

func NewRunner(
	privateDirectory string,
	request LeaseRequest,
	target ResolvedTarget,
) (openssh.RunSSH, error) {
	return newRunner(privateDirectory, request, target, runnerOptions{})
}

func newRunner(
	privateDirectory string,
	request LeaseRequest,
	target ResolvedTarget,
	options runnerOptions,
) (openssh.RunSSH, error) {
	projection := executionProjection(target)
	environment := credentials.StripEnvironment(request.Environment, []string{
		"SSH_ASKPASS",
		"SSH_ASKPASS_REQUIRE",
		"DISPLAY",
		askpassHandleEnvironment,
	})
	run := options.Run
	if run == nil {
		run = runSSHProcess
	}
	version := options.Version
	if version == nil {
		version = NewVersionPolicy(func(ctx context.Context) (string, error) {
			return runSSHVersion(ctx, request.WorkingDirectory, environment)
		})
	}
	return func(ctx context.Context, managerArguments []string) (int, error) {
		arguments, cleanup, err := materializeProjection(privateDirectory, projection)
		if err != nil {
			return -1, err
		}
		askpass, err := NewAskpass(ctx, privateDirectory, AskpassOptions{
			Version:     version,
			Executable:  options.Executable,
			Environment: environment,
			Prompt:      request.Prompt,
		})
		if err != nil {
			return -1, errors.Join(err, cleanup())
		}
		arguments = append(arguments, managerArguments...)
		exitCode, runErr := run(
			ctx,
			arguments,
			request.WorkingDirectory,
			askpass.Environment(),
		)
		closeErr := askpass.Close()
		return exitCode, errors.Join(runErr, askpass.Err(), closeErr, cleanup())
	}, nil
}

func runSSHVersion(
	ctx context.Context,
	workingDirectory string,
	environment []string,
) (string, error) {
	stdout, stderr, _, err := runOutput(
		ctx,
		[]string{"ssh", "-V"},
		workingDirectory,
		environment,
		nil,
	)
	return string(stderr) + string(stdout), err
}

func executionProjection(target ResolvedTarget) ExecutionProjection {
	projection := target.Projection
	if target.StrictHostKeyChecking != "" {
		projection.Arguments = append(
			append([]string(nil), projection.Arguments...),
			"-o", "StrictHostKeyChecking="+target.StrictHostKeyChecking,
		)
	}
	return projection
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
