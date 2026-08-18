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
	terminal, terminalBacked := clientTerminal(command)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ownerGroup := syscall.Getpgrp()
	childForeground := false
	restoreGroup := 0
	if terminalBacked {
		terminalForegroundMu.Lock()
		defer terminalForegroundMu.Unlock()
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
		foregroundGroup, err := terminalForegroundProcessGroup(terminal)
		if err != nil {
			return false, fmt.Errorf("inspect terminal before SSH client start: %w", err)
		}
		restoreGroup = foregroundGroup
		defer func() { _ = setTerminalForeground(terminal, restoreGroup) }()
		if foregroundGroup == ownerGroup {
			command.SysProcAttr.Foreground = true
			command.SysProcAttr.Ctty = int(terminal.Fd())
			childForeground = true
		}
	}
	if err := command.Start(); err != nil {
		return false, err
	}
	if terminalBacked {
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
	if !terminalBacked {
		return true, command.Wait()
	}
	return true, waitInteractiveClient(
		command,
		terminal,
		ownerGroup,
		childForeground,
		&restoreGroup,
	)
}

func clientTerminal(command *exec.Cmd) (*os.File, bool) {
	streams := []any{command.Stdin, command.Stdout, command.Stderr}
	for _, stream := range streams {
		file, ok := stream.(*os.File)
		if ok && term.IsTerminal(int(file.Fd())) {
			return file, true
		}
	}
	return nil, false
}

func waitInteractiveClient(
	command *exec.Cmd,
	terminal *os.File,
	ownerGroup int,
	childForeground bool,
	restoreGroup *int,
) error {
	childGroup := command.Process.Pid
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
			if childForeground {
				if err := setTerminalForeground(terminal, ownerGroup); err != nil {
					return fmt.Errorf("restore terminal before suspension: %w", err)
				}
				childForeground = false
			}
			// SIGSTOP is not discarded for an orphaned process group, which
			// matters for terminal emulators and PTY owners without a shell in
			// the same session. Stop the complete shell job so pipeline siblings
			// cannot keep running while kwt is suspended.
			if err := syscall.Kill(-ownerGroup, syscall.SIGSTOP); err != nil {
				return fmt.Errorf("suspend SSH client owner group: %w", err)
			}
			foregroundGroup, err := terminalForegroundProcessGroup(terminal)
			if err != nil {
				return fmt.Errorf("inspect terminal after SSH client resume: %w", err)
			}
			*restoreGroup = foregroundGroup
			if foregroundGroup == ownerGroup {
				if err := setTerminalForeground(terminal, childGroup); err != nil {
					return fmt.Errorf("restore SSH client foreground process group: %w", err)
				}
				childForeground = true
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

func terminalForegroundProcessGroup(terminal *os.File) (int, error) {
	return unix.IoctlGetInt(int(terminal.Fd()), unix.TIOCGPGRP)
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
	if e.status.Signaled() {
		return 128 + int(e.status.Signal())
	}
	return -1
}
