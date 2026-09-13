//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
)

func TestExecuteCancelsProjectRecoveryOnInterrupt(t *testing.T) {
	if os.Getenv("KWT_TEST_SIGNAL_CONTEXT") == "1" {
		readyPath := os.Getenv("KWT_TEST_SIGNAL_READY")
		canceledPath := os.Getenv("KWT_TEST_SIGNAL_CANCELED")
		home := os.Getenv("KWT_HOME")
		projectPath := filepath.Join(home, "missing")
		require.NoError(t, os.MkdirAll(home, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
			fmt.Appendf(nil, "[[projects]]\nrepository = 'github.com/acme/widget'\npath = %q\n", projectPath), 0o600))
		resolveDoctorProjectIdentity = func(ctx context.Context, _ config.ProjectRegistration) (string, error) {
			if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
				return "", err
			}
			<-ctx.Done()
			if err := os.WriteFile(canceledPath, []byte("canceled"), 0o600); err != nil {
				return "", err
			}
			return "", ctx.Err()
		}
		rootCmd.SetArgs([]string{"projects", "recover", projectPath})
		Execute()
		return
	}

	tmp := t.TempDir()
	readyPath := filepath.Join(tmp, "ready")
	canceledPath := filepath.Join(tmp, "canceled")
	command := exec.Command(os.Args[0], "-test.run=^TestExecuteCancelsProjectRecoveryOnInterrupt$")
	command.Env = append(os.Environ(),
		"KWT_TEST_SIGNAL_CONTEXT=1",
		"KWT_TEST_SIGNAL_READY="+readyPath,
		"KWT_TEST_SIGNAL_CANCELED="+canceledPath,
		"KWT_HOME="+filepath.Join(tmp, "kwt-home"),
	)
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "child command did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, command.Process.Signal(os.Interrupt))
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		assert.Equal(t, 130, exitErr.ExitCode())
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		require.FailNow(t, "command did not stop after interrupt")
	}
	assert.FileExists(t, canceledPath)
}

func TestExecuteDoesNotInterceptSignalForUncancelableCommand(t *testing.T) {
	if os.Getenv("KWT_TEST_UNCANCELABLE_SIGNAL") == "1" {
		readyPath := os.Getenv("KWT_TEST_SIGNAL_READY")
		waitCmd := &cobra.Command{
			Use: "wait-uncancelable-test-signal",
			RunE: func(_ *cobra.Command, _ []string) error {
				if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
					return err
				}
				select {}
			},
		}
		rootCmd.AddCommand(waitCmd)
		rootCmd.SetArgs([]string{waitCmd.Use})
		Execute()
		return
	}

	tmp := t.TempDir()
	readyPath := filepath.Join(tmp, "ready")
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestExecuteDoesNotInterceptSignalForUncancelableCommand$",
	)
	command.Env = append(os.Environ(),
		"KWT_TEST_UNCANCELABLE_SIGNAL=1",
		"KWT_TEST_SIGNAL_READY="+readyPath,
		"KWT_HOME="+filepath.Join(tmp, "kwt-home"),
	)
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
		}
	})
	waitForSignalTestFile(t, readyPath)
	require.NoError(t, command.Process.Signal(os.Interrupt))
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "the default handler should terminate the process")
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		require.FailNow(t, "uncancelable command swallowed the first interrupt")
	}
}

func TestExecuteRestoresDefaultSignalHandlingAfterCancel(t *testing.T) {
	if os.Getenv("KWT_TEST_SECOND_SIGNAL") == "1" {
		readyPath := os.Getenv("KWT_TEST_SIGNAL_READY")
		canceledPath := os.Getenv("KWT_TEST_SIGNAL_CANCELED")
		waitCmd := &cobra.Command{
			Use: "wait-for-second-test-signal",
			RunE: withGracefulSignals(func(cmd *cobra.Command, _ []string) error {
				if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
					return err
				}
				<-cmd.Context().Done()
				if err := os.WriteFile(canceledPath, []byte("canceled"), 0o600); err != nil {
					return err
				}
				select {}
			}),
		}
		rootCmd.AddCommand(waitCmd)
		rootCmd.SetArgs([]string{waitCmd.Use})
		Execute()
		return
	}

	tmp := t.TempDir()
	readyPath := filepath.Join(tmp, "ready")
	canceledPath := filepath.Join(tmp, "canceled")
	command := exec.Command(os.Args[0], "-test.run=^TestExecuteRestoresDefaultSignalHandlingAfterCancel$")
	command.Env = append(os.Environ(),
		"KWT_TEST_SECOND_SIGNAL=1",
		"KWT_TEST_SIGNAL_READY="+readyPath,
		"KWT_TEST_SIGNAL_CANCELED="+canceledPath,
		"KWT_HOME="+filepath.Join(tmp, "kwt-home"),
	)
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
		}
	})
	waitForSignalTestFile(t, readyPath)
	require.NoError(t, command.Process.Signal(os.Interrupt))
	waitForSignalTestFile(t, canceledPath)
	require.NoError(t, command.Process.Signal(os.Interrupt))
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "the restored default handler should terminate the process")
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		require.FailNow(t, "second interrupt did not force command termination")
	}
}

func waitForSignalTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "child command did not create expected file", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
