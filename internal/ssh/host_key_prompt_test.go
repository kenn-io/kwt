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
ECDSA key fingerprint is: SHA256:relay-fixture
This key is not known by any other names.
Are you sure you want to continue connecting (yes/no/[fingerprint])? `,
			expectation: hostKeyPromptDetails{
				Host:        "relay.example.test (100.64.0.7)",
				Algorithm:   "ECDSA",
				Fingerprint: "SHA256:relay-fixture",
			},
		},
		{
			name: "host known by another address",
			message: `The authenticity of host 'build.example.test (<no hostip for proxy command>)' can't be established.
ED25519 key fingerprint is: SHA256:build-fixture
This host key is known by the following other names/addresses:
    /tmp/known_hosts:7: [127.0.0.1]:2200
Are you sure you want to continue connecting (yes/no/[fingerprint])? `,
			expectation: hostKeyPromptDetails{
				Host:        "build.example.test (<no hostip for proxy command>)",
				Algorithm:   "ED25519",
				Fingerprint: "SHA256:build-fixture",
			},
		},
		{
			name: "matching SSHFP record",
			message: `The authenticity of host 'build.example.test' can't be established.
ED25519 key fingerprint is SHA256:dns-fixture.
Matching host key fingerprint found in DNS.
Are you sure you want to continue connecting (yes/no/[fingerprint])? `,
			expectation: hostKeyPromptDetails{
				Host:        "build.example.test",
				Algorithm:   "ED25519",
				Fingerprint: "SHA256:dns-fixture",
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
	for _, message := range []string{
		"Continue connecting?",
		`Password for The authenticity of host 'build.example.test' can't be established.
ED25519 key fingerprint is SHA256:fixture.
Are you sure you want to continue connecting (yes/no/[fingerprint])? `,
		`The authenticity of host 'build.example.test' can't be established.
ED25519 key fingerprint is SHA256:fixture.`,
		`The authenticity of host 'build.example.test' can't be established.
ED25519 key fingerprint is SHA256:fixture.
Are you sure you want to continue connecting (yes/no/[fingerprint])?
Password:`,
		`The authenticity of host 'build.example.test' can't be established.
ED25519 key fingerprint is SHA256:fixture.
Password:
Are you sure you want to continue connecting (yes/no/[fingerprint])? `,
	} {
		_, err := parseHostKeyPrompt(message)

		require.Error(t, err)
	}
}
