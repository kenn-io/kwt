package ssh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	runner, err := newRunner(privateDirectory, request, target, func(
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
	})
	require.NoError(t, err)

	exitCode, err := runner(context.Background(), []string{
		"-MNf", "-S", "/tmp/control", "--", "deploy@build.internal",
	})

	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, []string{"PATH=/usr/bin", "SSH_AUTH_SOCK=/tmp/agent"}, capturedEnvironment)
	assert.Equal(t, "-F", capturedArguments[0])
	assert.NotEqual(t, os.DevNull, capturedArguments[1])
	assert.Contains(t, capturedArguments, "HostName=build.internal")
	assert.True(t, strings.HasSuffix(strings.Join(capturedArguments, " "),
		"-MNf -S /tmp/control -- deploy@build.internal"))
	assert.NoFileExists(t, capturedArguments[1])
}
