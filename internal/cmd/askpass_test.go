package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const askpassExecuteHelper = "KWT_TEST_EXECUTE_ASKPASS"

func TestExecuteDispatchesAskpassBeforeCobra(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestExecuteAskpassHelperProcess")
	command.Env = append(os.Environ(),
		askpassExecuteHelper+"=1",
		"KWT_SSH_ASKPASS_HANDLE=invalid",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
}

func TestExecuteAskpassHelperProcess(t *testing.T) {
	if os.Getenv(askpassExecuteHelper) != "1" {
		return
	}
	os.Args = []string{"kwt", "Password:"}
	Execute()
	os.Exit(99)
}
