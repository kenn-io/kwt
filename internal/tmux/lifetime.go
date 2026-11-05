package tmux

import (
	"context"
	"fmt"
	"strings"
)

const workspaceDestroyOption = "@kwt-destroy-unattached"

// holdWorkspaceLifetime runs in the same command queue as new-session, before
// tmux can destroy the still-unattached session between bootstrap commands.
func holdWorkspaceLifetime(session string) []string {
	target := "=" + session + ":"
	return []string{";", "if-shell", "-F", "-t", target,
		"#{?#{==:#{destroy-unattached},off},0,#{destroy-unattached}}",
		"set-option -F -t " + quoteTmuxCommandArgument(target) + " " + workspaceDestroyOption +
			" '#{destroy-unattached}' ; set-option -t " + quoteTmuxCommandArgument(target) + " destroy-unattached off",
	}
}

// restoreWorkspaceLifetime lets any ordinary tmux client finish the handoff,
// including a client that switches from another session. The temporary hooks
// become inert after restoration and preserve session or inherited user hooks.
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
	case "1", "on", "keep-last", "keep-group":
	default:
		return fmt.Errorf("invalid pending workspace lifetime policy %q", policy)
	}
	target := "=" + session + ":"
	quoted := quoteTmuxCommandArgument(target)
	restore := "set-option -t " + quoted + " destroy-unattached " + policy +
		" ; set-option -u -t " + quoted + " " + workspaceDestroyOption
	for _, hook := range []string{"client-attached", "client-session-changed"} {
		output, err := r.tmux.RunCommandOutputContext(ctx, "show-options", "-q", "-t", target, hook)
		if err != nil {
			return err
		}
		if strings.TrimSpace(output) == "" {
			// A local hook shadows the global list. Copy inherited commands so
			// they still run with the real event's client and session context.
			output, err = r.tmux.RunCommandOutputContext(ctx, "show-options", "-gv", hook)
			if err != nil {
				return err
			}
			for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
				if line == "" {
					continue
				}
				if err := r.tmux.RunCommandContext(ctx, "set-hook", "-a", "-t", target, hook, line); err != nil {
					return err
				}
			}
		}
		command := "if-shell -F -t " + quoted + " '#{" + workspaceDestroyOption + "}' " + quoteTmuxCommandArgument(restore)
		if err := r.tmux.RunCommandContext(ctx, "set-hook", "-a", "-t", target, hook, command); err != nil {
			return err
		}
	}
	// A client may have visited before either hook was installed. Check the
	// attachment history after both hooks exist so that visit is also honored.
	return r.tmux.RunCommandContext(ctx, "if-shell", "-F", "-t", target,
		"#{session_last_attached}", restore)
}
