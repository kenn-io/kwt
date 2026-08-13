//go:build windows

package tmux

import (
	"context"
	"fmt"
)

func suspendRemovalProcessSessions(
	context.Context,
	[]int,
	map[int]struct{},
) ([]int, error) {
	return nil, fmt.Errorf("quiescing tmux pane processes is unsupported on Windows")
}

func resumeRemovalProcessGroups([]int) error { return nil }

func terminateRemovalProcessGroups(context.Context, []int) error {
	return fmt.Errorf("terminating quiesced tmux pane processes is unsupported on Windows")
}

func suspendRemovalServer(int) error {
	return fmt.Errorf("quiescing the tmux server is unsupported on Windows")
}

func resumeRemovalServer(int) error { return nil }
