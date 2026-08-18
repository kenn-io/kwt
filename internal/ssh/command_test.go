package ssh

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const clientProcessHelper = "KWT_SSH_CLIENT_PROCESS_HELPER"

func TestRunClientProcessStreamsStandardIO(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode, err := RunClientProcess(
		context.Background(),
		os.Args[0],
		[]string{"-test.run=^TestSSHClientProcessHelper$"},
		t.TempDir(),
		append(os.Environ(), clientProcessHelper+"=1"),
		strings.NewReader("request body"),
		&stdout,
		&stderr,
	)
	require.NoError(t, err)
	assert.Zero(t, exitCode)
	assert.Equal(t, "stdout:request body", stdout.String())
	assert.Equal(t, "stderr:request body", stderr.String())
}

func TestSSHClientProcessHelper(t *testing.T) {
	if os.Getenv(clientProcessHelper) == "" {
		return
	}
	value, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.WriteString("stdout:" + string(value))
	_, _ = os.Stderr.WriteString("stderr:" + string(value))
	os.Exit(0)
}
