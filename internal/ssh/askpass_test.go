package ssh

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestAskpassCarriesMultipleBoundRoundsIncludingEmptyResponse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "askpass")
	executable, err := os.Executable()
	require.NoError(t, err)
	responses := []string{"wrong", ""}
	var prompts []service.OperationPrompt
	transport, err := NewAskpass(context.Background(), root, AskpassOptions{
		Version:    supportedAskpassVersion(),
		Executable: executable,
		Environment: []string{
			"PATH=/usr/bin",
			"GHOSTHUB_AUTH=secret",
			"SSH_ASKPASS=/old/helper",
			"SSH_ASKPASS_PROMPT=confirm",
		},
		ProtectedNames: []string{"GHOSTHUB_AUTH"},
		Prompt: func(_ context.Context, prompt service.OperationPrompt) (string, error) {
			prompts = append(prompts, prompt)
			return responses[len(prompts)-1], nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	environment := transport.Environment()
	assert.Contains(t, environment, "SSH_ASKPASS="+executable)
	assert.Contains(t, environment, "SSH_ASKPASS_REQUIRE=force")
	assert.False(t, slices.Contains(environment, "GHOSTHUB_AUTH=secret"))
	assert.False(t, slices.Contains(environment, "SSH_ASKPASS=/old/helper"))
	assert.False(t, slices.Contains(environment, "SSH_ASKPASS_PROMPT=confirm"))

	var first bytes.Buffer
	exitCode, handled := RunAskpassHelper(
		[]string{"kwt", "Password for deploy@build:"},
		environment,
		&first,
	)
	assert.True(t, handled)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "wrong\n", first.String())

	var second bytes.Buffer
	exitCode, handled = RunAskpassHelper(
		[]string{"kwt", "Password for deploy@build:"},
		environment,
		&second,
	)
	assert.True(t, handled)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "\n", second.String())
	require.Len(t, prompts, 2)
	assert.Equal(t, "ssh_authentication", prompts[0].Kind)
	assert.True(t, prompts[0].Sensitive)
}

func TestAskpassTreatsUnhintedHostKeyTextAsSensitive(t *testing.T) {
	var captured service.OperationPrompt
	transport, err := NewAskpass(context.Background(), t.TempDir(), AskpassOptions{
		Version: supportedAskpassVersion(),
		Prompt: func(_ context.Context, prompt service.OperationPrompt) (string, error) {
			captured = prompt
			return "credential", nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	var output bytes.Buffer
	exitCode, handled := RunAskpassHelper(
		[]string{"kwt", `The authenticity of host 'example.test' can't be established.
ED25519 key fingerprint is SHA256:fixture.
Are you sure you want to continue connecting (yes/no/[fingerprint])? `},
		transport.Environment(),
		&output,
	)

	assert.True(t, handled)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "credential\n", output.String())
	assert.Equal(t, "ssh_authentication", captured.Kind)
	assert.True(t, captured.Sensitive)
}

func TestAskpassPreservesTypedPromptRejection(t *testing.T) {
	transport, err := NewAskpass(context.Background(), t.TempDir(), AskpassOptions{
		Version: supportedAskpassVersion(),
		Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "", service.NewError(
				service.SSHPromptRejected,
				"SSH prompt rejected",
				false,
				nil,
				nil,
			)
		},
	})
	require.NoError(t, err)

	exitCode, handled := RunAskpassHelper(
		[]string{"kwt", "Password:"},
		transport.Environment(),
		&bytes.Buffer{},
	)
	assert.True(t, handled)
	assert.NotEqual(t, 0, exitCode)
	require.NoError(t, transport.Close())
	assert.True(t, service.IsCode(transport.Err(), service.SSHPromptRejected))
}

func TestAskpassTimesOutOnePromptRound(t *testing.T) {
	deadlines := make(chan time.Time, 1)
	transport, err := NewAskpass(context.Background(), t.TempDir(), AskpassOptions{
		Version:       supportedAskpassVersion(),
		PromptTimeout: 10 * time.Millisecond,
		Prompt: func(ctx context.Context, prompt service.OperationPrompt) (string, error) {
			if prompt.Deadline != nil {
				deadlines <- *prompt.Deadline
			}
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	require.NoError(t, err)

	exitCode, handled := RunAskpassHelper(
		[]string{"kwt", "Password:"},
		transport.Environment(),
		&bytes.Buffer{},
	)
	assert.True(t, handled)
	assert.NotEqual(t, 0, exitCode)
	require.NoError(t, transport.Close())
	assert.True(t, service.IsCode(transport.Err(), service.SSHPromptTimedOut))
	select {
	case deadline := <-deadlines:
		assert.WithinDuration(t, time.Now(), deadline, time.Second)
	default:
		t.Fatal("askpass prompt did not carry its deadline")
	}
}

func TestAskpassRemovesPrivateOperationDirectory(t *testing.T) {
	root := t.TempDir()
	transport, err := NewAskpass(context.Background(), root, AskpassOptions{
		Version: supportedAskpassVersion(),
		Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "secret", nil
		},
	})
	require.NoError(t, err)
	directory := transport.directory
	require.DirExists(t, directory)

	require.NoError(t, transport.Close())
	assert.NoDirExists(t, directory)
}

func TestAskpassRejectsUnsupportedOpenSSHBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	transport, err := NewAskpass(context.Background(), root, AskpassOptions{
		Version: NewVersionPolicy(func(context.Context) (string, error) {
			return "OpenSSH_8.3p1", nil
		}),
		Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "secret", nil
		},
	})

	assert.Nil(t, transport)
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHUnsupportedVersion))
	entries, readErr := os.ReadDir(root)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestAskpassCancellationCreatesNoState(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	transport, err := NewAskpass(ctx, root, AskpassOptions{
		Version: supportedAskpassVersion(),
		Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "secret", nil
		},
	})

	assert.Nil(t, transport)
	require.ErrorIs(t, err, context.Canceled)
	entries, readErr := os.ReadDir(root)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestAskpassRejectsRelativeExecutableBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	transport, err := NewAskpass(context.Background(), root, AskpassOptions{
		Version:    supportedAskpassVersion(),
		Executable: "kwt",
		Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "secret", nil
		},
	})

	assert.Nil(t, transport)
	require.Error(t, err)
	entries, readErr := os.ReadDir(root)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func supportedAskpassVersion() *VersionPolicy {
	return NewVersionPolicy(func(context.Context) (string, error) {
		return "OpenSSH_9.9p1", nil
	})
}

func TestAskpassHelperIgnoresOrdinaryKwtInvocation(t *testing.T) {
	exitCode, handled := RunAskpassHelper(
		[]string{"kwt", "list"},
		os.Environ(),
		&bytes.Buffer{},
	)
	assert.False(t, handled)
	assert.Equal(t, 0, exitCode)
}

func TestAskpassHelperSubprocessReturnsPromptResponse(t *testing.T) {
	transport, err := NewAskpass(context.Background(), t.TempDir(), AskpassOptions{
		Version:    supportedAskpassVersion(),
		Executable: os.Args[0],
		Prompt: func(context.Context, service.OperationPrompt) (string, error) {
			return "subprocess-secret", nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	command := exec.Command(os.Args[0], "-test.run=TestAskpassProtocolHelperProcess")
	command.Env = append(transport.Environment(),
		"GOCOVERDIR="+t.TempDir(),
		"KWT_TEST_ASKPASS_PROTOCOL_HELPER=1",
		"KWT_TEST_ASKPASS_PROMPT=Password:",
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()

	require.NoError(t, err)
	assert.Equal(t, "subprocess-secret\n", string(output))
	assert.Empty(t, stderr.String())
}

func TestAskpassProtocolHelperProcess(t *testing.T) {
	if os.Getenv("KWT_TEST_ASKPASS_PROTOCOL_HELPER") != "1" {
		return
	}
	os.Args = []string{"kwt", os.Getenv("KWT_TEST_ASKPASS_PROMPT")}
	exitCode, handled := RunAskpassHelper(os.Args, os.Environ(), os.Stdout)
	if !handled {
		os.Exit(98)
	}
	os.Exit(exitCode)
}
