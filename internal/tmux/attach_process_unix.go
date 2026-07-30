//go:build !windows

package tmux

import (
	"os/exec"
	"syscall"
)

func replaceAttachProcess(cmd *exec.Cmd) error {
	return syscall.Exec(cmd.Path, cmd.Args, cmd.Env)
}
