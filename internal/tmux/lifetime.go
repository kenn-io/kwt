package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const workspaceDestroyOption = "@kwt-destroy-unattached"

// holdWorkspaceLifetime runs in the same command queue as new-session, before
// tmux can destroy the still-unattached session between bootstrap commands.
func holdWorkspaceLifetime(session string) []string {
	target := "=" + session + ":"
	return []string{";", "if-shell", "-F", "-t", target,
		"#{!=:#{destroy-unattached},off}",
		"set-option -F -t " + quoteTmuxCommandArgument(target) + " " + workspaceDestroyOption +
			" '#{destroy-unattached}' ; set-option -t " + quoteTmuxCommandArgument(target) + " destroy-unattached off",
	}
}

// restoreWorkspaceLifetime lets any ordinary tmux client finish the handoff,
// including a client that switches from another session. The temporary hooks
// remove themselves and preserve existing session or inherited user hooks.
func (r *WorkspaceRunner) restoreWorkspaceLifetime(ctx context.Context, session string) error {
	policy, err := r.tmux.sessionUserOption(ctx, session, workspaceDestroyOption)
	if err != nil {
		return err
	}
	policy = strings.TrimSpace(policy)
	if policy == "" {
		return nil
	}
	switch policy {
	case "on", "keep-last", "keep-group":
	default:
		return fmt.Errorf("invalid pending workspace lifetime policy %q", policy)
	}
	target := "=" + session + ":"
	quoted := quoteTmuxCommandArgument(target)
	for _, hook := range []string{"client-attached", "client-session-changed"} {
		output, err := r.tmux.RunCommandOutputContext(ctx, "show-options", "-q", "-t", target, hook)
		if err != nil {
			return err
		}
		index := 0
		local := strings.TrimSpace(output) != ""
		for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
			if line == "" {
				continue
			}
			key, _, ok := strings.Cut(line, "]")
			if !ok {
				return fmt.Errorf("invalid tmux hook entry %q", line)
			}
			value, err := strconv.Atoi(strings.TrimPrefix(key, hook+"["))
			if err != nil || value < 0 {
				return fmt.Errorf("invalid tmux hook index %q", key)
			}
			index = max(index, value+1)
		}
		key := fmt.Sprintf("%s[%d]", hook, index)
		remove := key
		if !local {
			remove = hook
		}
		restore := "set-option -t " + quoted + " destroy-unattached " + policy +
			" ; set-option -u -t " + quoted + " " + workspaceDestroyOption
		command := "set-hook -u -t " + quoted + " " + remove +
			" ; if-shell -F -t " + quoted + " '#{" + workspaceDestroyOption + "}' " + quoteTmuxCommandArgument(restore)
		if !local {
			// Unsetting the temporary local array reveals the inherited hook again.
			command += " ; set-hook -R -t " + quoted + " " + hook
		}
		if err := r.tmux.RunCommandContext(ctx, "set-hook", "-t", target, key, command); err != nil {
			return err
		}
	}
	return nil
}
