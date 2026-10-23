//go:build !windows

package lifecycle

import (
	"os/exec"
	"os/user"
	"slices"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/require"
)

func TestSystemProcessStatusReportsUnreapedChildAsZombie(t *testing.T) {
	useHostProcessTable(t)
	command := exec.Command("true")
	require.NoError(t, command.Start())
	t.Cleanup(func() { _ = command.Wait() })
	candidate := systemWorktreeProcess{Process: &process.Process{Pid: int32(command.Process.Pid)}}

	require.Eventually(t, func() bool {
		statuses, err := candidate.StatusWithContext(t.Context())
		return err == nil && slices.Contains(statuses, process.Zombie)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestSystemProcessStatusFailsForReapedProcess(t *testing.T) {
	useHostProcessTable(t)
	command := exec.Command("true")
	require.NoError(t, command.Run())
	candidate := systemWorktreeProcess{Process: &process.Process{Pid: int32(command.Process.Pid)}}

	_, err := candidate.StatusWithContext(t.Context())

	require.Error(t, err)
}

func TestSystemProcessStatusReadsOtherUsersProcesses(t *testing.T) {
	useHostProcessTable(t)
	processes, err := process.ProcessesWithContext(t.Context())
	require.NoError(t, err)
	currentUser, err := user.Current()
	require.NoError(t, err)

	others, read := 0, 0
	for _, candidate := range processes {
		username, err := candidate.UsernameWithContext(t.Context())
		if err != nil || username == currentUser.Username {
			continue
		}
		others++
		statuses, err := systemWorktreeProcess{Process: candidate}.StatusWithContext(t.Context())
		if err == nil {
			require.NotEmpty(t, statuses, "PID %d", candidate.Pid)
			read++
		}
	}
	if others == 0 {
		t.Skip("no processes owned by other users")
	}
	require.Positive(t, read)
}
