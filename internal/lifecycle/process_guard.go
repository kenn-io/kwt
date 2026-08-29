package lifecycle

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
	"go.kenn.io/kwt/internal/utils"
)

type worktreeProcessConditionError struct{ pids []int32 }

type worktreeProcess interface {
	pid() int32
	CwdWithContext(context.Context) (string, error)
}

type systemWorktreeProcess struct{ *process.Process }

func (p systemWorktreeProcess) pid() int32 { return p.Pid }

func (e *worktreeProcessConditionError) Error() string {
	values := processIDStrings(e.pids)
	if len(values) == 1 {
		return fmt.Sprintf(
			"worktree is in use by a process with its working directory inside it (PID %s); stop the process or rerun with --force",
			values[0],
		)
	}
	return fmt.Sprintf(
		"worktree is in use by processes with working directories inside it (PIDs %s); stop the processes or rerun with --force",
		strings.Join(values, ", "),
	)
}

func processIDStrings(pids []int32) []string {
	values := make([]string, len(pids))
	for index, pid := range pids {
		values[index] = fmt.Sprintf("%d", pid)
	}
	return values
}

func rejectProcessesUsingWorktree(ctx context.Context, path string) error {
	processes, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return fmt.Errorf("inspect process working directories: %w", err)
	}
	candidates := make([]worktreeProcess, len(processes))
	for index, candidate := range processes {
		candidates[index] = systemWorktreeProcess{Process: candidate}
	}
	return rejectProcessCandidatesUsingWorktree(ctx, path, candidates)
}

func rejectProcessCandidatesUsingWorktree(
	ctx context.Context,
	path string,
	processes []worktreeProcess,
) error {
	pids := make([]int32, 0)
	for _, candidate := range processes {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only positive evidence blocks removal. Working directories the
		// operating system refuses to disclose stay out of the verdict:
		// every systemd Linux user session owns capability-bearing processes
		// (systemd --user, (sd-pam)) whose /proc cwd is unreadable even to
		// the same user, so failing closed here would reject every default
		// removal on such hosts.
		workingDirectory, err := candidate.CwdWithContext(ctx)
		if err != nil || workingDirectory == "" {
			continue
		}
		if utils.IsSameOrChildPath(workingDirectory, path) {
			pids = append(pids, candidate.pid())
		}
	}
	if len(pids) > 0 {
		slices.Sort(pids)
		return &worktreeProcessConditionError{pids: pids}
	}
	return nil
}
