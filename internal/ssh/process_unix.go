//go:build !windows

package ssh

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

var terminalForegroundMu sync.Mutex

func runResolverCommand(command *exec.Cmd) error {
	_, err := runClientCommand(command)
	return err
}

func runClientCommand(command *exec.Cmd) (bool, error) {
	terminal, interactive := command.Stdin.(*os.File)
	interactive = interactive && term.IsTerminal(int(terminal.Fd()))
	if interactive {
		terminalForegroundMu.Lock()
		defer terminalForegroundMu.Unlock()
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
		command.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true, Foreground: true, Ctty: int(terminal.Fd()),
		}
	} else {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
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
	err := command.Wait()
	if interactive {
		if restoreErr := unix.IoctlSetPointerInt(
			int(terminal.Fd()), unix.TIOCSPGRP, syscall.Getpgrp(),
		); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore terminal foreground process group: %w", restoreErr)
		}
	}
	return true, err
}
