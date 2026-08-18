//go:build !windows

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
)

func TestSSHResolveSubprocessUsesFramedAccountLoginShell(t *testing.T) {
	binary := buildDaemonTestBinary(t, daemonTestBuild{
		Name: "kwt-ssh-resolve", Version: "v1.8.0", Revision: strings.Repeat("f", 40),
	})
	home := newDaemonTestHome(t, validDaemonConfig)
	fakeBin := filepath.Join(home, "bin")
	require.NoError(t, os.Mkdir(fakeBin, 0o700))
	fakeSSH := filepath.Join(fakeBin, "ssh")
	require.NoError(t, os.WriteFile(fakeSSH, []byte(`#!/bin/sh
printf '%s\n' \
  'host build.example.test' \
  'user deploy' \
  'hostname build.internal' \
  'port 2200' \
  'identityfile /keys/id one' \
  'ciphers aes256-gcm@openssh.com' \
  'localforward 8080 localhost:80'
printf '%s\n' 'KWT_SSH_CONFIG_START_forged_stderr' >&2
`), 0o700))
	profile := "echo account-login-banner\nexport PATH='" + fakeBin + "':$PATH\n"
	for _, name := range []string{".zprofile", ".bash_profile", ".profile"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, name), []byte(profile), 0o600))
	}
	registerDaemonCleanup(t, binary, home)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx, binary, "ssh", "resolve", "build.example.test", "--json",
	)
	command.Env = sshSubprocessEnvironment(home, fakeBin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		require.NoError(t, ctx.Err(), "stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	require.NoError(t, err, "stdout=%s stderr=%s", stdout.String(), stderr.String())
	var snapshot kwt.SSHRouteSnapshot
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &snapshot))
	require.Len(t, snapshot.Targets, 1)
	assert.Equal(t, "deploy@build.internal:2200", snapshot.Targets[0].DisplayTarget)
	assert.Contains(t, snapshot.Targets[0].Projection.Arguments, "Ciphers=aes256-gcm@openssh.com")
	assert.NotContains(t, stdout.String(), "account-login-banner")
	assert.NotContains(t, stdout.String(), "localforward")
	assert.Empty(t, stderr.String())

	for _, test := range []struct {
		name string
		args []string
		code string
	}{
		{name: "missing hostname", args: []string{"ssh", "resolve", "--json"}, code: "invalid_request"},
		{name: "invalid port", args: []string{"ssh", "resolve", "build.example.test", "--port", "-1", "--json"}, code: "ssh_invalid_target"},
		{name: "missing exec command", args: []string{"ssh", "exec", "--json", "build.example.test"}, code: "invalid_request"},
		{name: "missing copy destination", args: []string{"ssh", "copy", "--json", "build.example.test", "artifact"}, code: "invalid_request"},
		{name: "invalid exec flag", args: []string{"ssh", "exec", "--json", "--bogus"}, code: "invalid_request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := exec.Command(binary, test.args...)
			failure.Env = sshSubprocessEnvironment(home, fakeBin)
			var failureStdout, failureStderr bytes.Buffer
			failure.Stdout = &failureStdout
			failure.Stderr = &failureStderr
			err := failure.Run()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "stderr=%s", failureStderr.String())
			assert.Equal(t, 2, exitErr.ExitCode())
			assert.NotEqual(t, 255, exitErr.ExitCode())
			var envelope jsonErrorEnvelope
			require.NoError(t, json.Unmarshal(failureStdout.Bytes(), &envelope))
			assert.Equal(t, test.code, string(envelope.Error.Code))
		})
	}
}

func sshSubprocessEnvironment(home, fakeBin string) []string {
	environment := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || name == "KWT_HOME" || name == "PATH" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"HOME="+home,
		"KWT_HOME="+home,
		"PATH="+fakeBin+":/usr/bin:/bin",
	)
}
