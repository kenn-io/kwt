//go:build !windows

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func suspendRemovalProcessSessions(
	ctx context.Context,
	panePIDs []int,
	seen map[int]struct{},
) ([]int, error) {
	output, err := exec.CommandContext(ctx, "ps", "-e", "-o", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return nil, fmt.Errorf("inspect tmux pane process trees: %w", err)
	}
	type process struct{ group int }
	processes := make(map[int]process)
	children := make(map[int][]int)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		group, groupErr := strconv.Atoi(fields[2])
		if pidErr != nil || parentErr != nil || groupErr != nil || pid <= 1 || group <= 1 {
			continue
		}
		processes[pid] = process{group: group}
		children[parent] = append(children[parent], pid)
	}
	pending := append([]int(nil), panePIDs...)
	descendants := make(map[int]struct{}, len(panePIDs))
	for len(pending) > 0 {
		pid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, ok := descendants[pid]; ok {
			continue
		}
		descendants[pid] = struct{}{}
		pending = append(pending, children[pid]...)
	}
	groups := make([]int, 0)
	currentGroup := syscall.Getpgrp()
	for pid := range descendants {
		entry, ok := processes[pid]
		if !ok {
			continue
		}
		group := entry.group
		if _, ok := seen[group]; ok {
			continue
		}
		if group == currentGroup {
			return groups, fmt.Errorf("refuse to suspend the kwt process group")
		}
		if err := syscall.Kill(-group, syscall.SIGSTOP); err != nil {
			return groups, fmt.Errorf("suspend tmux pane process group %d: %w", group, err)
		}
		seen[group] = struct{}{}
		groups = append(groups, group)
	}
	if len(groups) == 0 && len(seen) == 0 {
		return nil, fmt.Errorf("suspend tmux pane processes: no process groups found")
	}
	return groups, nil
}

func suspendRemovalServer(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		return fmt.Errorf("suspend tmux server %d: %w", pid, err)
	}
	return nil
}

func resumeRemovalServer(pid int) error {
	if pid == 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("resume tmux server %d: %w", pid, err)
	}
	return nil
}

func resumeRemovalProcessGroups(groups []int) error {
	var result error
	for index := len(groups) - 1; index >= 0; index-- {
		if err := syscall.Kill(-groups[index], syscall.SIGCONT); err != nil &&
			!errors.Is(err, syscall.ESRCH) {
			result = errors.Join(result, fmt.Errorf(
				"resume tmux pane process group %d: %w", groups[index], err,
			))
		}
	}
	return result
}
