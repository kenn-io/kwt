//go:build windows

package tmux

import (
	"os"
	"os/exec"
)

func replaceAttachProcess(cmd *exec.Cmd) error {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
