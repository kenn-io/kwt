package tmux

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

const workspaceDestroyOption = "@kwt-destroy-unattached"
const workspaceLifetimeReadyOption = "@kwt-lifetime-hooks-ready"
const workspaceCreationOption = "@kwt-creating"

// The hook event supplies the session, so the global entries restore only it.
// The marker records the local policy, or inherit when it matches the global one.
const workspaceRestoreCommand = `if-shell -F "#{` + workspaceDestroyOption + `}" { if-shell -F "#{==:#{` + workspaceDestroyOption + `},inherit}" ` +
	`{ set-option -u destroy-unattached } { set-option -F destroy-unattached "#{` + workspaceDestroyOption + `}" } ; set-option -u ` + workspaceDestroyOption + " }"

func (t *TmuxCommand) commandPath() (string, error) {
	path, err := exec.LookPath(t.command)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

// Read global hooks explicitly: a session-local array hides them from formats.
// Prefix their commands in place so the real event retains its client context.
func workspaceLifetimeHooks(session, executable string) string {
	script := "set -e\n" +
		"tmux() { " + strings.ReplaceAll(quotePOSIXShellArgument(executable), "#", "##") + " -S #{q:socket_path} \"$@\"; }\n" +
		"restore=" + strings.ReplaceAll(quotePOSIXShellArgument(workspaceRestoreCommand), "#", "##") + "\n" +
		"target=" + strings.ReplaceAll(quotePOSIXShellArgument("="+session+":"), "#", "##") + "\n" +
		"for hook in client-attached client-session-changed; do\n" +
		"for scope in global local; do\n" +
		"if [ \"$scope\" = global ]; then\n" +
		"entry=$(tmux show-options -gqv \"$hook[0]\")\n" +
		"else\n" +
		"local_hooks=$(tmux show-options -q -t \"$target\" \"$hook\")\n" +
		"[ \"$local_hooks\" ] || continue\n" +
		"entry=$(tmux show-options -qv -t \"$target\" \"$hook[0]\")\n" +
		"fi\n" +
		"case \"$entry\" in \"$restore\"*) continue ;; esac\n" +
		"commands=$restore\n" +
		"[ ! \"$entry\" ] || commands=\"$commands ; $entry\"\n" +
		"if [ \"$scope\" = global ]; then\n" +
		"tmux set-hook -g \"$hook[0]\" \"$commands\"\n" +
		"else\n" +
		"tmux set-hook -t \"$target\" \"$hook[0]\" \"$commands\"\n" +
		"fi\ndone\ndone"
	target := quoteTmuxCommandArgument("=" + session + ":")
	// Older tmux versions do not propagate run-shell's exit status. Keep the
	// installation result on the held session until the caller checks it.
	return "set-option -t " + target + " " + workspaceLifetimeReadyOption + " 0 ; if-shell " +
		quoteTmuxCommandArgument(script) + " { set-option -t " + target + " " + workspaceLifetimeReadyOption + " 1 }"
}

// holdWorkspaceLifetime runs in the same command queue as new-session, before
// tmux can destroy the still-unattached session between bootstrap commands.
func holdWorkspaceLifetime(session, executable string) []string {
	target := "=" + session + ":"
	return []string{";", "if-shell", "-F", "-t", target,
		"#{?#{==:#{destroy-unattached},off},0,#{destroy-unattached}}",
		"set-option -F -t " + quoteTmuxCommandArgument(target) + " " + workspaceDestroyOption + " '#{destroy-unattached}' ; set-option -u -t " + quoteTmuxCommandArgument(target) + " destroy-unattached" +
			" ; if-shell -F -t " + quoteTmuxCommandArgument(target) + " '#{==:#{destroy-unattached},#{" + workspaceDestroyOption + "}}' { set-option -t " + quoteTmuxCommandArgument(target) + " " + workspaceDestroyOption + " inherit }" +
			" ; set-option -t " + quoteTmuxCommandArgument(target) + " destroy-unattached off ; " + workspaceLifetimeHooks(session, executable),
	}
}

// restoreWorkspaceLifetime lets any ordinary tmux client finish the handoff,
// including a client that switches from another session. Already attached
// clients keep their policy until another attachment runs the repaired hooks.
func (r *WorkspaceRunner) restoreWorkspaceLifetime(ctx context.Context, session string) error {
	pending, err := r.tmux.sessionUserOption(ctx, session, workspaceDestroyOption)
	if err != nil {
		return err
	}
	if strings.TrimSpace(pending) == "" {
		return nil
	}
	executable, err := r.tmux.commandPath()
	if err != nil {
		return err
	}
	if err := r.tmux.RunCommandContext(ctx, "if-shell", "-F", "1", workspaceLifetimeHooks(session, executable)); err != nil {
		return err
	}
	return r.checkWorkspaceLifetimeHooks(ctx, session)
}

func (r *WorkspaceRunner) checkWorkspaceLifetimeHooks(ctx context.Context, session string) error {
	ready, err := r.tmux.sessionUserOption(ctx, session, workspaceLifetimeReadyOption)
	if err != nil {
		return err
	}
	if strings.TrimSpace(ready) == "" {
		return nil
	}
	if strings.TrimSpace(ready) != "1" {
		return errors.New("cannot install workspace lifetime hooks")
	}
	return r.tmux.RunCommandContext(ctx, "set-option", "-u", "-t", "="+session+":", workspaceLifetimeReadyOption)
}
