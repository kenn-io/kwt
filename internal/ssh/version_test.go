package ssh

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestParseOpenSSHVersionAcceptsPortableAndAppleOutput(t *testing.T) {
	tests := []struct {
		output string
		want   OpenSSHVersion
	}{
		{"OpenSSH_8.4p1, OpenSSL 1.1.1w", OpenSSHVersion{Major: 8, Minor: 4}},
		{"OpenSSH_9.9p1, LibreSSL 3.3.6", OpenSSHVersion{Major: 9, Minor: 9}},
		{"OpenSSH_for_Windows_9.5p1, LibreSSL 3.8.2", OpenSSHVersion{Major: 9, Minor: 5}},
		{"OpenSSH 10.1 vendor-build", OpenSSHVersion{Major: 10, Minor: 1}},
	}
	for _, test := range tests {
		version, err := parseOpenSSHVersion(test.output)
		require.NoError(t, err, test.output)
		assert.Equal(t, test.want, version)
	}
}

func TestParseOpenSSHVersionRejectsUnrelatedVersionText(t *testing.T) {
	_, err := parseOpenSSHVersion("Dropbear v2025.88")
	require.Error(t, err)
}

func TestVersionPolicyRequiresOpenSSH84ForInteractiveUse(t *testing.T) {
	policy := NewVersionPolicy(func(context.Context) (string, error) {
		return "OpenSSH_8.3p1, LibreSSL 3.3.6", nil
	})

	err := policy.RequireInteractive(context.Background())

	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHUnsupportedVersion))
}

func TestVersionPolicyCachesAuthoritativeResult(t *testing.T) {
	calls := 0
	policy := NewVersionPolicy(func(context.Context) (string, error) {
		calls++
		return "OpenSSH_8.4p1", nil
	})

	require.NoError(t, policy.RequireInteractive(context.Background()))
	require.NoError(t, policy.RequireInteractive(context.Background()))
	assert.Equal(t, 1, calls)
}

func TestVersionPolicyPreservesCancellationWithoutCachingIt(t *testing.T) {
	calls := 0
	policy := NewVersionPolicy(func(context.Context) (string, error) {
		calls++
		return "OpenSSH_9.0p1", nil
	})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	err := policy.RequireInteractive(canceled)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	require.NoError(t, policy.RequireInteractive(context.Background()))
	assert.Equal(t, 1, calls)
}
