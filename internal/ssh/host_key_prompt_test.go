package ssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseHostKeyPrompt(t *testing.T) {
	tests := []struct {
		name        string
		message     string
		expectation hostKeyPromptDetails
	}{
		{
			name: "portable OpenSSH",
			message: `The authenticity of host 'build.example.test' can't be established.
ED25519 key fingerprint is SHA256:fixture.
Are you sure you want to continue connecting (yes/no/[fingerprint])? `,
			expectation: hostKeyPromptDetails{
				Host:        "build.example.test",
				Algorithm:   "ED25519",
				Fingerprint: "SHA256:fixture",
			},
		},
		{
			name: "host with effective address and colon marker",
			message: `The authenticity of host 'relay.example.test (100.64.0.7)' can't be established.
ECDSA key fingerprint is: SHA256:relay-fixture.
Are you sure you want to continue connecting (yes/no/[fingerprint])? `,
			expectation: hostKeyPromptDetails{
				Host:        "relay.example.test (100.64.0.7)",
				Algorithm:   "ECDSA",
				Fingerprint: "SHA256:relay-fixture",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual, err := parseHostKeyPrompt(test.message)

			require.NoError(t, err)
			assert.Equal(t, test.expectation, actual)
		})
	}
}

func TestParseHostKeyPromptRejectsUnstructuredConfirmation(t *testing.T) {
	_, err := parseHostKeyPrompt("Continue connecting?")

	require.Error(t, err)
}
