package lifecycle

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

// Process states from <sys/proc.h>.
const (
	darwinProcessRunning  = 2
	darwinProcessSleeping = 3
	darwinProcessStopped  = 4
	darwinProcessZombie   = 5
)

// StatusWithContext reads the kernel process state directly. gopsutil reads it
// from ps, which prints a blank state for processes it cannot fully read, and
// gopsutil v4.26.9 then indexes an empty result and panics.
func (p systemWorktreeProcess) StatusWithContext(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", int(p.Pid))
	if err != nil {
		return nil, fmt.Errorf("read status of process %d: %w", p.Pid, err)
	}
	switch info.Proc.P_stat {
	case darwinProcessRunning:
		return []string{process.Running}, nil
	case darwinProcessSleeping:
		return []string{process.Sleep}, nil
	case darwinProcessStopped:
		return []string{process.Stop}, nil
	case darwinProcessZombie:
		return []string{process.Zombie}, nil
	default:
		return []string{process.UnknownState}, nil
	}
}
