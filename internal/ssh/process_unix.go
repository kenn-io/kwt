//go:build !windows

package ssh

import (
	"context"
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

func newClientCommand(_ context.Context, executable string, arguments ...string) *exec.Cmd {
	return exec.Command(executable, arguments...)
}

func runResolverCommand(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return killProcessGroup(command) }
	if err := command.Start(); err != nil {
		return err
	}
	return command.Wait()
}

func runClientCommand(ctx context.Context, command *exec.Cmd) (bool, error) {
	terminal, interactive := command.Stdin.(*os.File)
	interactive = interactive && term.IsTerminal(int(terminal.Fd()))
	if err := ctx.Err(); err != nil {
		return false, err
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if interactive {
		terminalForegroundMu.Lock()
		defer terminalForegroundMu.Unlock()
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
		command.SysProcAttr.Foreground = true
		command.SysProcAttr.Ctty = int(terminal.Fd())
	}
	if err := command.Start(); err != nil {
		return false, err
	}
	if interactive {
		defer command.Process.Release() //nolint:errcheck // wait4 below owns reaping.
	}
	cancelDone := make(chan struct{})
	cancelStopped := make(chan struct{})
	defer func() {
		close(cancelDone)
		<-cancelStopped
	}()
	go func() {
		defer close(cancelStopped)
		select {
		case <-ctx.Done():
			_ = killProcessGroup(command)
		case <-cancelDone:
		}
	}()
	if !interactive {
		return true, command.Wait()
	}
	return true, waitInteractiveClient(command, terminal)
}

func waitInteractiveClient(command *exec.Cmd, terminal *os.File) error {
	parentGroup := syscall.Getpgrp()
	childGroup := command.Process.Pid
	defer func() {
		_ = setTerminalForeground(terminal, parentGroup)
	}()
	for {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(
			command.Process.Pid,
			&status,
			syscall.WUNTRACED,
			nil,
		)
		if err != nil {
			return err
		}
		switch {
		case status.Stopped():
			if err := setTerminalForeground(terminal, parentGroup); err != nil {
				return fmt.Errorf("restore terminal before suspension: %w", err)
			}
			// SIGSTOP is not discarded for an orphaned process group, which
			// matters for terminal emulators and PTY owners without a shell in
			// the same session. The shell still observes kwt as stopped and can
			// resume it with the ordinary foreground-job path.
			if err := syscall.Kill(os.Getpid(), syscall.SIGSTOP); err != nil {
				return fmt.Errorf("suspend SSH client owner: %w", err)
			}
			if err := setTerminalForeground(terminal, childGroup); err != nil {
				return fmt.Errorf("restore SSH client foreground process group: %w", err)
			}
			if err := syscall.Kill(-childGroup, syscall.SIGCONT); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("continue SSH client process group: %w", err)
			}
		case status.Exited():
			if status.ExitStatus() == 0 {
				return nil
			}
			return clientWaitError{status: status}
		case status.Signaled():
			return clientWaitError{status: status}
		}
	}
}

func setTerminalForeground(terminal *os.File, processGroup int) error {
	return unix.IoctlSetPointerInt(
		int(terminal.Fd()), unix.TIOCSPGRP, processGroup,
	)
}

func killProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

type clientWaitError struct{ status syscall.WaitStatus }

func (e clientWaitError) Error() string {
	if e.status.Signaled() {
		return fmt.Sprintf("SSH client terminated by signal %s", e.status.Signal())
	}
	return fmt.Sprintf("SSH client exited with status %d", e.status.ExitStatus())
}

func (e clientWaitError) ExitCode() int {
	if e.status.Exited() {
		return e.status.ExitStatus()
	}
	return -1
}
