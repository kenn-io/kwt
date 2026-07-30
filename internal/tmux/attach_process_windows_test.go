//go:build windows

package tmux

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

const windowsAttachProcessHelper = "KWT_TEST_WINDOWS_ATTACH_PROCESS"

func TestReplaceAttachProcessWaitsOnWindows(t *testing.T) {
	if os.Getenv(windowsAttachProcessHelper) == "1" {
		os.Exit(23)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(
		executable,
		"-test.run=^TestReplaceAttachProcessWaitsOnWindows$",
	)
	helper.Env = append(os.Environ(), windowsAttachProcessHelper+"=1")
	err = replaceAttachProcess(helper)

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("replacement error = %v, want child exit error", err)
	}
	if exitErr.ExitCode() != 23 {
		t.Fatalf("replacement exit code = %d, want 23", exitErr.ExitCode())
	}
}
