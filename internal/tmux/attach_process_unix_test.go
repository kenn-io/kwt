//go:build !windows

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

const unixAttachProcessHelper = "KWT_TEST_UNIX_ATTACH_PROCESS"

func TestReplaceAttachProcessReplacesUnixProcess(t *testing.T) {
	if os.Getenv(unixAttachProcessHelper) == "1" {
		cmd := exec.Command(
			"sh",
			"-c",
			`test "$$" = "$KWT_TEST_UNIX_ATTACH_PID" && printf replaced`,
		)
		cmd.Env = append(
			os.Environ(),
			"KWT_TEST_UNIX_ATTACH_PID="+strconv.Itoa(os.Getpid()),
		)
		if err := replaceAttachProcess(cmd); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "replace process: %v", err)
			os.Exit(41)
		}
		_, _ = fmt.Fprint(os.Stderr, "replacement returned")
		os.Exit(42)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(
		executable,
		"-test.run=^TestReplaceAttachProcessReplacesUnixProcess$",
	)
	helper.Env = append(os.Environ(), unixAttachProcessHelper+"=1")
	output, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("replacement helper failed: %v: %s", err, output)
	}
	if string(output) != "replaced" {
		t.Fatalf("replacement output = %q, want %q", output, "replaced")
	}
}
