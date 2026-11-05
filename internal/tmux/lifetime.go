package tmux

import (
	"context"
	"strings"
)

const workspaceDestroyOption = "@kwt-destroy-unattached"

// Leave global entries intact when a local array hides their ownership.
const workspaceLifetimeCleanup = `if-shell -F "#{==:#{S:#{` + workspaceDestroyOption + `}},}" { ` +
	`if-shell -F "#{m:if-shell -F *set-option -u ` + workspaceDestroyOption + `*,#{client-attached[999]}}" { set-hook -gu client-attached[999] } ; ` +
	`if-shell -F "#{m:if-shell -F *set-option -u ` + workspaceDestroyOption + `*,#{client-session-changed[999]}}" { set-hook -gu client-session-changed[999] } }`

// The hook event supplies the session, so the global entries restore only it.
// The marker records the local policy, or inherit when it matches the global one.
const workspaceRestoreCommand = `if-shell -F "#{` + workspaceDestroyOption + `}" { if-shell -F "#{==:#{` + workspaceDestroyOption + `},inherit}" ` +
	`{ set-option -u destroy-unattached } { set-option -F destroy-unattached "#{` + workspaceDestroyOption + `}" } ; set-option -u ` + workspaceDestroyOption + " ; " + workspaceLifetimeCleanup + " }"

// User commands take priority over the shared restore hook.
const workspaceLifetimeHooks = `set-option -gqo client-attached[999] '` + workspaceRestoreCommand + `' ; ` +
	`set-option -gqo client-session-changed[999] '` + workspaceRestoreCommand + `'`

// holdWorkspaceLifetime runs in the same command queue as new-session, before
// tmux can destroy the still-unattached session between bootstrap commands.
func holdWorkspaceLifetime(session string) []string {
	target := "=" + session + ":"
	return []string{";", "if-shell", "-F", "-t", target,
		"#{?#{==:#{destroy-unattached},off},0,#{destroy-unattached}}",
		"set-option -F -t " + quoteTmuxCommandArgument(target) + " " + workspaceDestroyOption + " '#{destroy-unattached}' ; set-option -u -t " + quoteTmuxCommandArgument(target) + " destroy-unattached" +
			" ; if-shell -F -t " + quoteTmuxCommandArgument(target) + " '#{==:#{destroy-unattached},#{" + workspaceDestroyOption + "}}' { set-option -t " + quoteTmuxCommandArgument(target) + " " + workspaceDestroyOption + " inherit }" +
			" ; set-option -t " + quoteTmuxCommandArgument(target) + " destroy-unattached off ; " + workspaceLifetimeHooks,
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
	if err := r.tmux.RunCommandContext(ctx, "if-shell", "-F", "1", workspaceLifetimeHooks); err != nil {
		return err
	}
	target := "=" + session + ":"
	for _, hook := range []string{"client-attached", "client-session-changed"} {
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
