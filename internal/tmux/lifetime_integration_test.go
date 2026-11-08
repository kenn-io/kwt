//go:build !windows

package tmux

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
)

type lifetimeAttachmentTmux struct {
	*TmuxCommand
	afterCreate func()
}

func (command *lifetimeAttachmentTmux) RunCommandOutputContext(ctx context.Context, args ...string) (string, error) {
	output, err := command.TmuxCommand.RunCommandOutputContext(ctx, args...)
	if err == nil && args[0] == "new-session" {
		command.afterCreate()
	}
	return output, err
}

func TestWorkspaceWithDestroyUnattachedOff(t *testing.T) {
	fixture := newRealWorkspaceSessionsFixture(t)
	ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
	defer cancel()
	command := fixture.servers.kwtServer()
	require.NoError(t, command.RunCommandContext(ctx, "-f", "/dev/null", "new-session", "-d", "-s", "unrelated"))
	require.NoError(t, command.RunCommandContext(ctx, "set-option", "-g", "destroy-unattached", "off"))
	before, err := command.RunCommandOutputContext(ctx, "show-hooks", "-g")
	require.NoError(t, err)
	setWorkspaceServerPathWithoutTmux(t, ctx, command)
	runner := NewWorkspaceRunner(command, nil)
	for _, session := range []string{"workspace", "second-workspace"} {
		require.NoError(t, runner.Ensure(ctx, session, fixture.workspace, BlankLayout()))
		policy, err := command.sessionUserOption(ctx, session, workspaceDestroyOption)
		require.NoError(t, err)
		require.Empty(t, strings.TrimSpace(policy))
		output, err := command.RunCommandOutputContext(ctx, "show-options", "-q", "-t", session, "client-attached")
		require.NoError(t, err)
		require.Empty(t, strings.TrimSpace(output))
		require.True(t, command.HasSession(session))
		output, err = command.RunCommandOutputContext(ctx, "list-panes", "-a", "-F", "#{pane_start_command}")
		require.NoError(t, err)
		require.NotContains(t, output, "sleep", "Ensure must replace its placeholder with the user's shell")
		output, err = command.RunCommandOutputContext(ctx, "show-hooks", "-g")
		require.NoError(t, err)
		require.Equal(t, before, output, "workspaces with the hold disabled must leave global hooks alone")
	}
}

func setWorkspaceServerPathWithoutTmux(t *testing.T, ctx context.Context, command *TmuxCommand) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	require.NoError(t, err)
	bin := t.TempDir()
	require.NoError(t, os.Symlink(sleep, filepath.Join(bin, "sleep")))
	require.NoError(t, command.RunCommandContext(ctx, "set-option", "-g", "default-shell", "/bin/sh", ";", "set-environment", "-g", "PATH", bin))
}

func TestWorkspaceSurvivesBootstrapWithDestroyUnattached(t *testing.T) {
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	for _, tc := range []struct {
		name, installedHook, reopen                                                                     string
		warm, switchClient, localHooks, earlyAttach, followGlobal, localPolicy, user999, restrictedPath bool
		visited                                                                                         bool
	}{
		{name: "fresh server"},
		{name: "server PATH without tmux", warm: true, restrictedPath: true},
		{name: "existing session hooks", localHooks: true},
		{name: "switch client", warm: true, switchClient: true},
		{name: "attach before local restore installation", earlyAttach: true, localHooks: true},
		{name: "global policy change", followGlobal: true},
		{name: "session-local policy", localPolicy: true},
		{name: "reload before reopen", reopen: "ensure"},
		{name: "reload before guarded reopen", reopen: "repair"},
		{name: "reopen after a detached visit", reopen: "ensure", visited: true},
		{name: "guarded reopen after a detached visit", reopen: "repair", visited: true},
		{name: "reload with session hooks", reopen: "ensure", localHooks: true},
		{name: "reload before switching client", reopen: "ensure", warm: true, switchClient: true},
		{name: "user hooks at 999", user999: true},
		{name: "user hooks at 999 with local hooks", user999: true, localHooks: true, warm: true},
		{name: "installed local hook", installedHook: "client-attached"},
		{name: "installed local session-changed hook", installedHook: "client-session-changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRealWorkspaceSessionsFixture(t)
			ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
			defer cancel()
			root := fixture.workspace
			config := filepath.Join(root, "tmux.conf")
			// Keep server shutdown separate from the session lifetime assertions.
			contents := "set -s exit-empty off\nset -g destroy-unattached on\nset -g default-shell /bin/sh\nset-hook -g client-attached 'set -g @user-attach-hook ran'\nset-hook -g client-session-changed 'set -g @user-switch-hook ran'\n" +
				"set-hook -g client-attached[3] 'set -gF @user-attach-client \"#{hook_client}\"'\n" +
				"set-hook -g client-session-changed[3] 'set -gF @user-switch-client \"#{hook_client}\"'\n"
			updatedHook := "client-attached"
			if tc.installedHook != "" {
				if tc.installedHook == "client-attached" {
					updatedHook = "client-session-changed"
				}
				contents += `set-hook -g ` + tc.installedHook + `[5] { set-hook -t workspace ` + tc.installedHook + `[0] { set -gF @followup-zero "#{hook_client}" ; set -gF @user-attach-client "#{hook_client}" }; set-hook -t workspace ` + tc.installedHook + `[9] { set -gF @followup "#{hook_client}" }; set -g @installed yes }` + "\n"
			}
			if tc.localHooks {
				contents += "set-hook -g after-new-session { if-shell -F '#{==:#{session_name},workspace}' { set-hook client-attached[4] { set -g @user-attach-hook local }; set-hook client-session-changed[7] { set -g @user-switch-hook local }; set-hook client-attached[9] { set -gF @user-attach-client \"#{hook_client}\" }; set-hook client-session-changed[9] { set -gF @user-switch-client \"#{hook_client}\" } } }\n"
			}
			if tc.localPolicy {
				contents += "set -g destroy-unattached off\nset-hook -g after-new-session { set-option destroy-unattached on }\n"
			}
			if tc.user999 {
				contents += "set-hook -g client-attached[999] { set -gF @user-999-client \"#{hook_client}\" }\nset-hook -g client-session-changed[999] { set -g @user-999-switch yes }\n"
			}
			require.NoError(t, os.WriteFile(config, []byte(contents), 0600))
			wrapper := filepath.Join(root, "tmux")
			require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quotePOSIXShellArgument(binary)+" -f "+quotePOSIXShellArgument(config)+" \"$@\"\n"), 0700))
			command := fixture.servers.kwtServer()
			command.command = wrapper
			if tc.warm {
				require.NoError(t, command.RunCommandContext(ctx, "new-session", "-d", "-s", "unrelated", "sleep", "30", ";", "set-option", "-t", "unrelated", "destroy-unattached", "off"))
			}
			if tc.restrictedPath {
				setWorkspaceServerPathWithoutTmux(t, ctx, command)
			}
			var globalHooks string
			if tc.user999 && tc.localHooks {
				globalHooks, err = command.RunCommandOutputContext(ctx, "show-hooks", "-g")
				require.NoError(t, err)
			}
			var terminal *os.File
			var client *exec.Cmd
			destination := "workspace"
			if tc.switchClient {
				destination = "unrelated"
			}
			attach := func() {
				client = command.attachSessionCmd(destination)
				client.Env = append(client.Env, "TERM=xterm-256color")
				var err error
				terminal, err = pty.Start(client)
				require.NoError(t, err)
				drained := make(chan struct{})
				attachedClient, attachedTerminal := client, terminal
				t.Cleanup(func() {
					_ = attachedTerminal.Close()
					_ = attachedClient.Process.Kill()
					_ = attachedClient.Wait()
					<-drained
				})
				go func() { defer close(drained); _, _ = io.Copy(io.Discard, attachedTerminal) }()
				if tc.switchClient {
					var clientName string
					require.Eventually(t, func() bool {
						out, err := command.RunCommandOutputContext(ctx, "list-clients", "-F", "#{client_name}")
						clientName = strings.TrimSpace(out)
						return err == nil && clientName != ""
					}, 5*time.Second, 10*time.Millisecond)
					require.NoError(t, command.RunCommandContext(ctx, "set-option", "-gu", "@user-switch-hook"))
					require.NoError(t, command.RunCommandContext(ctx, "set-option", "-gu", "@user-switch-client"))
					require.NoError(t, command.RunCommandContext(ctx, "switch-client", "-c", clientName, "-t", "workspace"))
				}
				hook := "@user-attach-hook"
				if tc.switchClient {
					hook = "@user-switch-hook"
				}
				expected := "ran"
				if tc.localHooks {
					expected = "local"
				}
				require.Eventually(t, func() bool {
					out, err := command.RunCommandOutputContext(ctx, "show-options", "-gv", hook)
					return err == nil && strings.TrimSpace(out) == expected
				}, 5*time.Second, 10*time.Millisecond)
				clientName, err := command.RunCommandOutputContext(ctx, "list-clients", "-t", "workspace", "-F", "#{client_name}")
				require.NoError(t, err)
				require.NotEmpty(t, strings.TrimSpace(clientName))
				if tc.user999 && !tc.localHooks {
					require.Eventually(t, func() bool {
						output, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", "@user-999-client")
						return err == nil && strings.TrimSpace(output) == strings.TrimSpace(clientName)
					}, time.Second, 10*time.Millisecond)
					output, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", "@user-999-switch")
					require.NoError(t, err)
					require.Equal(t, "yes", strings.TrimSpace(output))
				}
				require.Eventually(t, func() bool {
					output, err := command.RunCommandOutputContext(ctx, "show-options", "-gv", strings.Replace(hook, "-hook", "-client", 1))
					return err == nil && strings.TrimSpace(output) == strings.TrimSpace(clientName)
				}, 5*time.Second, 10*time.Millisecond)
			}
			var backend workspaceTmux = command
			if tc.earlyAttach {
				backend = &lifetimeAttachmentTmux{TmuxCommand: command, afterCreate: attach}
			}
			runner := NewWorkspaceRunner(backend, nil)
			require.NoError(t, runner.Ensure(ctx, "workspace", root, BlankLayout()))
			if tc.reopen != "" {
				before, err := command.RunCommandOutputContext(ctx, "show-options", "-q", "-t", "workspace", "client-attached")
				require.NoError(t, err)
				require.NoError(t, command.RunCommandContext(ctx, "source-file", config))
				var panePID string
				if tc.visited {
					attach()
					panePID, err = command.RunCommandOutputContext(ctx, "display-message", "-p", "-t", "workspace", "#{pane_pid}")
					require.NoError(t, err)
					require.NotEmpty(t, strings.TrimSpace(panePID))
					require.NoError(t, client.Process.Kill())
					var exitErr *exec.ExitError
					require.ErrorAs(t, client.Wait(), &exitErr)
					require.Eventually(t, func() bool {
						output, err := command.RunCommandOutputContext(ctx, "list-clients", "-t", "workspace", "-F", "#{client_name}")
						return err == nil && strings.TrimSpace(output) == ""
					}, time.Second, 10*time.Millisecond)
				}
				for range 2 {
					if tc.reopen == "repair" {
						require.NoError(t, runner.RepairExisting(ctx, "workspace", root, BlankLayout()))
					} else {
						require.NoError(t, runner.Ensure(ctx, "workspace", root, BlankLayout()))
					}
					if tc.visited {
						after, err := command.RunCommandOutputContext(ctx, "display-message", "-p", "-t", "workspace", "#{pane_pid}")
						require.NoError(t, err, "reopening must preserve the detached workspace")
						require.Equal(t, panePID, after, "reopening must preserve the running pane process")
					}
				}
				if tc.localHooks {
					after, err := command.RunCommandOutputContext(ctx, "show-options", "-q", "-t", "workspace", "client-attached")
					require.NoError(t, err)
					require.Equal(t, before, after, "reopening must reuse existing session-local restore hooks")
				}
			}
			if tc.user999 {
				before, err := command.RunCommandOutputContext(ctx, "show-hooks", "-g")
				require.NoError(t, err)
				if tc.localHooks {
					require.Equal(t, globalHooks, before, "session-local arrays must not hide user-owned global slots")
				}
				require.NoError(t, runner.Ensure(ctx, "workspace", root, BlankLayout()))
				require.NoError(t, NewWorkspaceRunner(command, nil).Ensure(ctx, "spare", root, BlankLayout()))
				after, err := command.RunCommandOutputContext(ctx, "show-hooks", "-g")
				require.NoError(t, err)
				require.Equal(t, before, after, "workspace creation and reopening must reuse the shared hooks")
			}
			visits := 1
			if tc.installedHook != "" {
				visits = 2
				policy, err := command.RunCommandOutputContext(ctx, "show-options", "-Av", "-t", "workspace", "destroy-unattached")
				require.NoError(t, err)
				require.Equal(t, "off", strings.TrimSpace(policy))
			}
			for visit := range visits {
				if visit == 1 {
					require.NoError(t, command.RunCommandContext(ctx, "set-hook", "-g", updatedHook, `set -g @user-attach-hook ran ; set -gF @user-attach-client "#{hook_client}" ; set -gF @seen "new:#{hook_client}"`))
				}
				if !tc.earlyAttach || visit > 0 {
					attach()
				}
				out, err := command.RunCommandOutputContext(ctx, "show-options", "-Av", "-t", "workspace", "destroy-unattached")
				require.NoError(t, err)
				expectedPolicy := "on"
				if tc.installedHook != "" && visit > 0 {
					expectedPolicy = "off"
				}
				if !tc.user999 || tc.localHooks {
					require.Equal(t, expectedPolicy, strings.TrimSpace(out))
				}
				if tc.installedHook != "" {
					require.Eventually(t, func() bool {
						if visit == 0 {
							out, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", "@installed")
							return err == nil && strings.TrimSpace(out) == "yes"
						}
						clientName, err := command.RunCommandOutputContext(ctx, "list-clients", "-t", "workspace", "-F", "#{client_name}")
						if err != nil || strings.TrimSpace(clientName) == "" {
							return false
						}
						out, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", "@seen")
						if err != nil || strings.TrimSpace(out) != "new:"+strings.TrimSpace(clientName) {
							return false
						}
						for _, option := range []string{"@followup-zero", "@followup"} {
							out, err = command.RunCommandOutputContext(ctx, "show-options", "-gqv", option)
							if err != nil || strings.TrimSpace(out) != strings.TrimSpace(clientName) {
								return false
							}
						}
						return true
					}, 5*time.Second, 10*time.Millisecond)
				}
				marker := filepath.Join(root, fmt.Sprintf("shell-ready-%d", visit))
				_, err = fmt.Fprintf(terminal, "printf 'ready-%%s' worktree > %s\r", quotePOSIXShellArgument(marker))
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					contents, err := os.ReadFile(marker)
					return err == nil && string(contents) == "ready-worktree"
				}, 5*time.Second, 10*time.Millisecond)
				if tc.followGlobal || (tc.installedHook != "" && visit == 0) {
					require.NoError(t, command.RunCommandContext(ctx, "set-option", "-g", "destroy-unattached", "off"))
				}
				// Disconnect the client directly. Tmux 3.6's detach-client path can
				// redraw a session after destroy-unattached removes its last window.
				require.NoError(t, client.Process.Kill())
				var exitErr *exec.ExitError
				require.ErrorAs(t, client.Wait(), &exitErr)
				require.Equal(t, -1, exitErr.ExitCode())
				if tc.followGlobal || tc.installedHook != "" {
					require.True(t, command.HasSession("workspace"))
				} else if !tc.user999 || tc.localHooks {
					require.Eventually(t, func() bool { return !command.HasSession("workspace") }, 5*time.Second, 10*time.Millisecond)
				}
			}
			if tc.warm {
				exists, err := command.HasSessionContext(ctx, "unrelated")
				require.NoError(t, err)
				require.True(t, exists)
			}
			out, err := command.RunCommandOutputContext(ctx, "show-options", "-sv", "exit-empty")
			require.NoError(t, err, "the tmux server must survive the client disconnect")
			require.Equal(t, "off", strings.TrimSpace(out))
		})
	}
}
