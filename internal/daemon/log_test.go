package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestRotatingLogIsPrivateAndRetainsThreeBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	log, err := openRotatingLog(path, 16, 3)
	require.NoError(t, err)
	for value := range 6 {
		_, err = fmt.Fprintf(log, "entry-%02d\n", value)
		require.NoError(t, err)
	}
	require.NoError(t, log.Close())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.FileExists(t, path+".1")
	assert.FileExists(t, path+".2")
	assert.FileExists(t, path+".3")
	assert.NoFileExists(t, path+".4")
}

func TestServiceFailureLogBoundsAndRedactsPrivateCause(t *testing.T) {
	const environmentSecret = "environment-secret-value"
	const bearerSecret = "bearer-secret-value"
	cause := errors.New(
		"KWT_API_TOKEN=" + environmentSecret +
			"\nAuthorization: Bearer " + bearerSecret + "\n" +
			strings.Repeat("command-output-", 200),
	)
	failure := service.NewError(
		service.Internal, "internal failure", false, nil, cause,
	)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	logServiceFailure(
		logger,
		"/api/v1/inventory",
		failure,
		[]string{environmentSecret, bearerSecret},
	)

	assert.NotContains(t, output.String(), environmentSecret)
	assert.NotContains(t, output.String(), bearerSecret)
	var record map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	assert.Equal(t, "/api/v1/inventory", record["route"])
	assert.Equal(t, string(service.Internal), record["code"])
	diagnostic, ok := record["error"].(string)
	require.True(t, ok)
	assert.LessOrEqual(t, len(diagnostic), maximumDiagnosticBytes)
	assert.Contains(t, diagnostic, "[redacted]")
}

func TestPrivateDiagnosticRedactsAuthorizationAndURIUserinfo(t *testing.T) {
	for _, test := range []struct {
		name       string
		message    string
		secrets    []string
		wantSuffix string
	}{
		{
			name:    "basic authorization",
			message: "request failed\nAuthorization: Basic dXNlcjpwYXNz\ncontinued",
			secrets: []string{"Basic dXNlcjpwYXNz"}, wantSuffix: "\ncontinued",
		},
		{
			name:    "proxy authorization",
			message: "Proxy-Authorization: Negotiate opaque-credential\ncontinued",
			secrets: []string{"Negotiate opaque-credential"}, wantSuffix: "\ncontinued",
		},
		{
			name:    "ssh URI user information",
			message: "clone ssh://user:password@host/repo",
			secrets: []string{"user:password"},
		},
		{
			name:    "git ssh URI user information",
			message: "clone git+ssh://token@host/repo",
			secrets: []string{"token"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := privateDiagnostic(errors.New(test.message), nil)
			for _, secret := range test.secrets {
				assert.NotContains(t, got, secret)
			}
			assert.Contains(t, got, "[redacted]")
			if test.wantSuffix != "" {
				assert.True(t, strings.HasSuffix(got, test.wantSuffix), got)
			}
		})
	}
}

func TestPrivateDiagnosticRedactsStructureBeforeOverlappingValues(t *testing.T) {
	const headerCredential = "private-header-credential"
	got := privateDiagnostic(
		errors.New(
			"Authorization: Basic "+headerCredential+"\n"+
				"overlapping abcdef and abc",
		),
		[]string{"Authorization", "abc", "abcdef", "abc"},
	)

	assert.NotContains(t, got, headerCredential)
	assert.NotContains(t, got, "abcdef")
	assert.NotContains(t, got, "[redacted]def")
	assert.Contains(t, got, "[redacted]")
}

func TestRotatingLogRotatesAnOversizedExistingFileOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte("already-too-large"), 0o600))
	log, err := openRotatingLog(path, 4, 3)
	require.NoError(t, err)
	require.NoError(t, log.Close())
	assert.FileExists(t, path+".1")
}

func TestRotatingLogRejectsSymlinkedActivePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.log")
	require.NoError(t, os.WriteFile(target, []byte("preserve"), 0o600))
	path := filepath.Join(dir, "daemon.log")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := openRotatingLog(path, 16, 3)
	require.Error(t, err)
	body, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "preserve", string(body))
}
