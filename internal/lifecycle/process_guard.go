package lifecycle

import (
	"context"
	"fmt"
	"os/user"
	"slices"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
	"go.kenn.io/kwt/internal/utils"
)

type worktreeProcessConditionError struct{ pids []int32 }

type worktreeProcessInspectionError struct{ pids []int32 }

type worktreeProcess interface {
	pid() int32
	CwdWithContext(context.Context) (string, error)
	StatusWithContext(context.Context) ([]string, error)
	UsernameWithContext(context.Context) (string, error)
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

func (e *worktreeProcessInspectionError) Error() string {
	values := processIDStrings(e.pids)
	if len(values) == 1 {
		return fmt.Sprintf(
			"cannot verify whether process PID %s uses the worktree because its working directory could not be inspected; stop the process or rerun with --force",
			values[0],
		)
	}
	return fmt.Sprintf(
		"cannot verify whether processes with PIDs %s use the worktree because their working directories could not be inspected; stop the processes or rerun with --force",
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
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve current user for process inspection: %w", err)
	}
	pids := make([]int32, 0)
	uninspectablePIDs := make([]int32, 0)
	for _, candidate := range processes {
		if err := ctx.Err(); err != nil {
			return err
		}
		workingDirectory, err := candidate.CwdWithContext(ctx)
		if err != nil || workingDirectory == "" {
			statuses, statusErr := candidate.StatusWithContext(ctx)
			if statusErr == nil && slices.Contains(statuses, process.Zombie) {
				continue
			}
			username, usernameErr := candidate.UsernameWithContext(ctx)
			if usernameErr == nil && username == currentUser.Username {
				uninspectablePIDs = append(uninspectablePIDs, candidate.pid())
			}
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
	if len(uninspectablePIDs) > 0 {
		slices.Sort(uninspectablePIDs)
		return &worktreeProcessInspectionError{pids: uninspectablePIDs}
	}
	return nil
}
