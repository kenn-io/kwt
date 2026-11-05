package tmux

import (
	"context"
	"strings"
)

const workspaceDestroyOption = "@kwt-destroy-unattached"

// The hook event supplies the session, so the global entries restore only it.
// The marker records the local policy, or inherit when it matches the global one.
const workspaceRestoreCommand = "if-shell -F '#{" + workspaceDestroyOption + "}' { if-shell -F '#{==:#{" + workspaceDestroyOption + "},inherit}' " +
	"{ set-option -u destroy-unattached } { set-option -F destroy-unattached '#{" + workspaceDestroyOption + "}' } ; set-option -u " + workspaceDestroyOption + " }"

// holdWorkspaceLifetime runs in the same command queue as new-session, before
// tmux can destroy the still-unattached session between bootstrap commands.
func holdWorkspaceLifetime(session string) []string {
	target := "=" + session + ":"
	return []string{";", "if-shell", "-F", "-t", target,
		"#{?#{==:#{destroy-unattached},off},0,#{destroy-unattached}}",
		"set-option -F -t " + quoteTmuxCommandArgument(target) + " " + workspaceDestroyOption + " '#{destroy-unattached}' ; set-option -u -t " + quoteTmuxCommandArgument(target) + " destroy-unattached" +
			" ; if-shell -F -t " + quoteTmuxCommandArgument(target) + " '#{==:#{destroy-unattached},#{" + workspaceDestroyOption + "}}' { set-option -t " + quoteTmuxCommandArgument(target) + " " + workspaceDestroyOption + " inherit }" +
			" ; set-option -t " + quoteTmuxCommandArgument(target) + " destroy-unattached off" +
			" ; set-hook -g client-attached[999] { " + workspaceRestoreCommand + " }" +
			" ; set-hook -g client-session-changed[999] { " + workspaceRestoreCommand + " }",
	}
}

// restoreWorkspaceLifetime lets any ordinary tmux client finish the handoff,
// including a client that switches from another session. Session-local user
// hooks shadow the global entries installed during creation, so append to them.
func (r *WorkspaceRunner) restoreWorkspaceLifetime(ctx context.Context, session string) error {
	pending, err := r.tmux.sessionUserOption(ctx, session, workspaceDestroyOption)
	if err != nil {
		return err
	}
	if strings.TrimSpace(pending) == "" {
		return nil
	}
	target := "=" + session + ":"
	for _, hook := range []string{"client-attached", "client-session-changed"} {
		if err := r.tmux.RunCommandContext(ctx, "set-hook", "-g", hook+"[999]", workspaceRestoreCommand); err != nil {
			return err
		}
		output, err := r.tmux.RunCommandOutputContext(ctx, "show-options", "-q", "-t", target, hook)
		if err != nil {
			return err
		}
		if strings.TrimSpace(output) == "" {
			continue
		}
		if err := r.tmux.RunCommandContext(ctx, "set-hook", "-a", "-t", target, hook, workspaceRestoreCommand); err != nil {
			return err
		}
	}
	// A client may have visited before either hook was installed. Check the
	// attachment history after both hooks exist so that visit is also honored.
	return r.tmux.RunCommandContext(ctx, "if-shell", "-F", "-t", target,
		"#{session_last_attached}", workspaceRestoreCommand)
}
