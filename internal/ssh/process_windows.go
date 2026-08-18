//go:build windows

package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func newClientCommand(ctx context.Context, executable string, arguments ...string) *exec.Cmd {
	return exec.CommandContext(ctx, executable, arguments...)
}

func runResolverCommand(command *exec.Cmd) error {
	_, err := runClientCommand(context.Background(), command)
	return err
}

func runClientCommand(_ context.Context, command *exec.Cmd) (bool, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return false, fmt.Errorf("create resolver job: %w", err)
	}
	var closeJobOnce sync.Once
	closeJob := func() {
		closeJobOnce.Do(func() {
			_ = windows.CloseHandle(job)
		})
	}
	defer closeJob()

	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		return false, fmt.Errorf("configure resolver job: %w", err)
	}

	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	command.Cancel = func() error {
		closeJob()
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return command.Process.Kill()
	}
	if err := command.Start(); err != nil {
		return false, err
	}
	setupComplete := false
	defer func() {
		if setupComplete {
			return
		}
		closeJob()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()

	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false,
		uint32(command.Process.Pid),
	)
	if err != nil {
		return true, fmt.Errorf("open resolver process: %w", err)
	}
	processOpen := true
	defer func() {
		if processOpen {
			_ = windows.CloseHandle(process)
		}
	}()
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return true, fmt.Errorf("assign resolver job: %w", err)
	}
	if err := resumeResolverProcess(uint32(command.Process.Pid)); err != nil {
		return true, err
	}

	setupComplete = true
	processOpen = false
	go func() {
		_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
		_ = windows.CloseHandle(process)
		closeJob()
	}()
	return true, command.Wait()
}

func resumeResolverProcess(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot resolver threads: %w", err)
	}
	defer windows.CloseHandle(snapshot) //nolint:errcheck // Read-only snapshot cleanup.

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != processID {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fmt.Errorf("open resolver thread: %w", openErr)
		}
		_, resumeErr := windows.ResumeThread(thread)
		closeErr := windows.CloseHandle(thread)
		if resumeErr != nil {
			return fmt.Errorf("resume resolver thread: %w", resumeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close resolver thread: %w", closeErr)
		}
		return nil
	}
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerate resolver threads: %w", err)
	}
	return errors.New("resolver process has no primary thread")
}
