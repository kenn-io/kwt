//go:build !windows

package ssh

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/term"
)

func runResolverCommand(command *exec.Cmd) error {
	_, err := runClientCommand(command)
	return err
}

func runClientCommand(command *exec.Cmd) (bool, error) {
	terminal, interactive := command.Stdin.(*os.File)
	interactive = interactive && term.IsTerminal(int(terminal.Fd()))
	if !interactive {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if interactive {
			return command.Process.Kill()
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if err := command.Start(); err != nil {
		return false, err
	}
	return true, command.Wait()
}
