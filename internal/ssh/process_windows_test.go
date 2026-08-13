//go:build windows

package ssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const resolverProcessHelperRole = "KWT_RESOLVER_PROCESS_HELPER_ROLE"

func TestRunOutputCancellationTerminatesWindowsProcessTree(t *testing.T) {
	tempDir := t.TempDir()
	readyPath := filepath.Join(tempDir, "ready")
	startPath := filepath.Join(tempDir, "start")
	childPIDPath := filepath.Join(tempDir, "child-pid")
	environment := append(os.Environ(),
		resolverProcessHelperRole+"=parent",
		"KWT_RESOLVER_READY_PATH="+readyPath,
		"KWT_RESOLVER_START_PATH="+startPath,
		"KWT_RESOLVER_CHILD_PID_PATH="+childPIDPath,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := runOutput(
			ctx,
			[]string{os.Args[0], "-test.run=^TestResolverWindowsProcessHelper$"},
			tempDir,
			environment,
			nil,
		)
		done <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("resolver parent did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(startPath, nil, 0o600); err != nil {
		cancel()
		t.Fatal(err)
	}

	var childPIDBytes []byte
	for {
		var err error
		childPIDBytes, err = os.ReadFile(childPIDPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("resolver child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	childPID, err := strconv.ParseUint(strings.TrimSpace(string(childPIDBytes)), 10, 32)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_TERMINATE,
		false,
		uint32(childPID),
	)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		_ = windows.CloseHandle(handle)
	}()
	defer windows.TerminateProcess(handle, 1) //nolint:errcheck // Cleanup after a failed assertion.

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runOutput error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolver process did not stop after cancellation")
	}

	status, err := windows.WaitForSingleObject(handle, 5_000)
	if err != nil {
		t.Fatal(err)
	}
	if status != windows.WAIT_OBJECT_0 {
		t.Fatalf("resolver child remained alive after cancellation: wait status %d", status)
	}
}

func TestRunOutputResolvesExecutableWithInvocationEnvironment(t *testing.T) {
	daemonDir := t.TempDir()
	invocationDir := t.TempDir()
	const executable = "kwt-ssh-path-test.exe"
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{daemonDir, invocationDir} {
		if err := os.WriteFile(filepath.Join(directory, executable), binary, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", daemonDir)
	t.Setenv("PATHEXT", ".EXE")
	environment := append(
		os.Environ(),
		"PATH="+invocationDir,
		"PATHEXT=.EXE",
		resolverProcessHelperRole+"=lookup",
	)

	stdout, _, _, err := runOutput(
		context.Background(),
		[]string{"kwt-ssh-path-test", "-test.run=^TestResolverWindowsProcessHelper$"},
		t.TempDir(),
		environment,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(invocationDir, executable)
	if !strings.EqualFold(filepath.Clean(string(stdout)), filepath.Clean(want)) {
		t.Fatalf("resolver executable = %q, want %q", stdout, want)
	}
}

func TestResolverWindowsProcessHelper(t *testing.T) {
	switch os.Getenv(resolverProcessHelperRole) {
	case "":
		return
	case "parent":
		if err := os.WriteFile(os.Getenv("KWT_RESOLVER_READY_PATH"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(os.Getenv("KWT_RESOLVER_START_PATH")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("parent helper timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestResolverWindowsProcessHelper$")
		child.Env = append(os.Environ(), resolverProcessHelperRole+"=child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			os.Getenv("KWT_RESOLVER_CHILD_PID_PATH"),
			[]byte(strconv.Itoa(child.Process.Pid)),
			0o600,
		); err != nil {
			_ = child.Process.Kill()
			t.Fatal(err)
		}
		_ = child.Wait()
	case "child":
		for {
			time.Sleep(time.Second)
		}
	case "lookup":
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.WriteString(executable)
	default:
		t.Fatalf("unknown helper role %q", os.Getenv(resolverProcessHelperRole))
	}
}
