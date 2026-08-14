package ssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

const runnerAskpassHelper = "KWT_TEST_RUNNER_ASKPASS"

func TestRunnerAppliesProjectionAndPrivateConfigToEveryManagerCommand(t *testing.T) {
	privateDirectory := filepath.Join(t.TempDir(), "private")
	request := LeaseRequest{
		WorkingDirectory: t.TempDir(),
		Environment:      []string{"PATH=/usr/bin", "SSH_AUTH_SOCK=/tmp/agent"},
	}
	target := ResolvedTarget{Projection: ExecutionProjection{
		Arguments: []string{"-F", os.DevNull, "-o", "HostName=build.internal"},
		PrivateConfig: []string{
			`IdentityFile "/keys/build"`,
		},
	}}
	var capturedArguments []string
	var capturedEnvironment []string
	runner, err := newRunner(privateDirectory, request, target, runnerOptions{
		Version: supportedAskpassVersion(),
		Run: func(
			_ context.Context,
			arguments []string,
			_ string,
			environment []string,
		) (int, error) {
			capturedArguments = append([]string(nil), arguments...)
			capturedEnvironment = append([]string(nil), environment...)
			configIndex := -1
			for index, argument := range arguments {
				if argument == "-F" {
					configIndex = index + 1
					break
				}
			}
			require.GreaterOrEqual(t, configIndex, 1)
			contents, readErr := os.ReadFile(arguments[configIndex])
			require.NoError(t, readErr)
			assert.Equal(t, privateDirectory, filepath.Dir(arguments[configIndex]))
			assert.Equal(t, `IdentityFile "/keys/build"`+"\n", string(contents))
			return 0, nil
		},
	})
	require.NoError(t, err)

	exitCode, err := runner(context.Background(), []string{
		"-MNf", "-S", "/tmp/control", "--", "deploy@build.internal",
	})

	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Contains(t, capturedEnvironment, "PATH=/usr/bin")
	assert.Contains(t, capturedEnvironment, "SSH_AUTH_SOCK=/tmp/agent")
	assert.Contains(t, capturedEnvironment, "SSH_ASKPASS_REQUIRE=force")
	assert.Equal(t, "-F", capturedArguments[0])
	assert.NotEqual(t, os.DevNull, capturedArguments[1])
	assert.Contains(t, capturedArguments, "HostName=build.internal")
	assert.True(t, strings.HasSuffix(strings.Join(capturedArguments, " "),
		"-MNf -S /tmp/control -- deploy@build.internal"))
	assert.NoFileExists(t, capturedArguments[1])
}

func TestRunnerEnforcesResolvedHostKeyPolicyWithoutAmbientAskpass(t *testing.T) {
	request := LeaseRequest{
		WorkingDirectory: t.TempDir(),
		Environment: []string{
			"PATH=/usr/bin",
			"SSH_ASKPASS=/tmp/untrusted-helper",
			"SSH_ASKPASS_REQUIRE=force",
			"DISPLAY=:0",
			askpassHandleEnvironment + "=untrusted-handle",
		},
	}
	target := ResolvedTarget{
		StrictHostKeyChecking: "yes",
		Projection: ExecutionProjection{
			Arguments: []string{"-F", os.DevNull},
		},
	}
	var capturedArguments []string
	var capturedEnvironment []string
	runner, err := newRunner(t.TempDir(), request, target, runnerOptions{
		Version: supportedAskpassVersion(),
		Run: func(
			_ context.Context,
			arguments []string,
			_ string,
			environment []string,
		) (int, error) {
			capturedArguments = append([]string(nil), arguments...)
			capturedEnvironment = append([]string(nil), environment...)
			return 0, nil
		},
	})
	require.NoError(t, err)

	_, err = runner(context.Background(), []string{"-MNf", "--", "build.internal"})

	require.NoError(t, err)
	assert.Contains(t, capturedArguments, "StrictHostKeyChecking=yes")
	assert.Contains(t, capturedEnvironment, "PATH=/usr/bin")
	assert.Contains(t, capturedEnvironment, "SSH_ASKPASS_REQUIRE=force")
	assert.NotContains(t, capturedEnvironment, "SSH_ASKPASS=/tmp/untrusted-helper")
}

func TestRunnerCarriesPromptThroughAskpassHelperProcess(t *testing.T) {
	request := LeaseRequest{
		WorkingDirectory: t.TempDir(),
		Environment:      []string{"PATH=" + os.Getenv("PATH")},
		Prompt: func(_ context.Context, prompt service.OperationPrompt) (string, error) {
			assert.Equal(t, "Password:", prompt.Message)
			return "secret", nil
		},
	}
	target := ResolvedTarget{Projection: ExecutionProjection{
		Arguments: []string{"-F", os.DevNull},
	}}
	var output bytes.Buffer
	runner, err := newRunner(
		t.TempDir(),
		request,
		target,
		runnerOptions{
			Version: supportedAskpassVersion(),
			Run: func(
				_ context.Context,
				_ []string,
				_ string,
				environment []string,
			) (int, error) {
				command := exec.Command(os.Args[0], "-test.run=TestRunnerAskpassHelperProcess")
				command.Env = append(environment, runnerAskpassHelper+"=1")
				command.Stdout = &output
				err := command.Run()
				if err == nil {
					return 0, nil
				}
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return exitErr.ExitCode(), err
				}
				return -1, err
			},
		},
	)
	require.NoError(t, err)

	exitCode, err := runner(context.Background(), []string{"-MNf", "--", "build.internal"})

	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "secret\n", output.String())
}

func TestRunnerPropagatesPromptFailure(t *testing.T) {
	promptFailure := service.NewError(
		service.SSHPromptRejected,
		"SSH prompt was rejected",
		false,
		nil,
		nil,
	)
	request := LeaseRequest{
		WorkingDirectory: t.TempDir(),
		Environment:      []string{"PATH=" + os.Getenv("PATH")},
		Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "", promptFailure
		},
	}
	runner, err := newRunner(
		t.TempDir(),
		request,
		ResolvedTarget{Projection: ExecutionProjection{
			Arguments: []string{"-F", os.DevNull},
		}},
		runnerOptions{
			Version: supportedAskpassVersion(),
			Run: func(
				_ context.Context,
				_ []string,
				_ string,
				environment []string,
			) (int, error) {
				exitCode, handled := RunAskpassHelper(
					[]string{"kwt", "Password:"},
					environment,
					&bytes.Buffer{},
				)
				require.True(t, handled)
				return exitCode, errors.New("SSH prompt failed")
			},
		},
	)
	require.NoError(t, err)

	_, err = runner(context.Background(), []string{"-MNf", "--", "build.internal"})

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHPromptRejected))
}

func TestRunnerAskpassHelperProcess(t *testing.T) {
	if os.Getenv(runnerAskpassHelper) != "1" {
		return
	}
	exitCode, handled := RunAskpassHelper(
		[]string{"kwt", "Password:"},
		os.Environ(),
		os.Stdout,
	)
	if !handled {
		os.Exit(98)
	}
	os.Exit(exitCode)
}
