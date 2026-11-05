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
	"github.com/stretchr/testify/assert"
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
	runner := NewWorkspaceRunner(command, nil)
	require.NoError(t, runner.Ensure(ctx, "workspace", t.TempDir(), BlankLayout()))
	policy, err := command.sessionUserOption(ctx, "workspace", workspaceDestroyOption)
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(policy))
	output, err := command.RunCommandOutputContext(ctx, "show-options", "-q", "-t", "workspace", "client-attached")
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(output))
	require.True(t, command.HasSession("workspace"))
	output, err = command.RunCommandOutputContext(ctx, "show-hooks", "-g")
	require.NoError(t, err)
	require.Equal(t, before, output, "the first workspace must leave global hooks alone")
	require.NoError(t, runner.Ensure(ctx, "second-workspace", t.TempDir(), BlankLayout()))
	after, err := command.RunCommandOutputContext(ctx, "show-hooks", "-g")
	require.NoError(t, err)
	require.Equal(t, before, after, "workspaces with the hold disabled must leave global hooks alone")
}

func TestWorkspaceWithServerPathWithoutTmux(t *testing.T) {
	for _, policy := range []string{"off", "on"} {
		t.Run(policy, func(t *testing.T) {
			fixture := newRealWorkspaceSessionsFixture(t)
			sleep, err := exec.LookPath("sleep")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
			defer cancel()
			root := fixture.workspace
			bin := filepath.Join(root, "bin")
			require.NoError(t, os.Mkdir(bin, 0700))
			require.NoError(t, os.Symlink(sleep, filepath.Join(bin, "sleep")))
			command := fixture.servers.kwtServer()
			require.NoError(t, command.RunCommandContext(ctx, "-f", "/dev/null", "new-session", "-d", "-s", "unrelated", "/bin/sh"))
			require.NoError(t, command.RunCommandContext(ctx, "set-option", "-t", "unrelated", "destroy-unattached", "off", ";", "set-option", "-g", "destroy-unattached", policy, ";", "set-option", "-g", "default-shell", "/bin/sh", ";", "set-environment", "-g", "PATH", bin))
			err = NewWorkspaceRunner(command, nil).Ensure(ctx, "workspace", root, BlankLayout())
			assert.NoError(t, err)
			panes, err := command.RunCommandOutputContext(ctx, "list-panes", "-a", "-F", "#{session_name}:#{pane_start_command}")
			require.NoError(t, err)
			require.NotContains(t, panes, "sleep", "Ensure must replace its placeholder with the user's shell")
			require.True(t, command.HasSession("workspace"))
		})
	}
}

func TestWorkspaceSurvivesBootstrapWithDestroyUnattached(t *testing.T) {
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	for _, tc := range []struct {
		name, secondVisit, secondHook                                                                    string
		warm, switchClient, localHooks, earlyAttach, followGlobal, localPolicy, user999, multiplePending bool
	}{
		{name: "fresh server", multiplePending: true},
		{name: "existing server", warm: true},
		{name: "existing session hooks", localHooks: true},
		{name: "switch client", warm: true, switchClient: true},
		{name: "attach before local restore installation", earlyAttach: true, localHooks: true},
		{name: "switch before local restore installation", warm: true, switchClient: true, earlyAttach: true, localHooks: true},
		{name: "global policy change", followGlobal: true},
		{name: "session-local policy", localPolicy: true},
		{name: "user hooks at 999", user999: true},
		{name: "user hooks at 999 with local hooks", user999: true, localHooks: true, warm: true},
		{name: "updated global hook", secondVisit: "global"},
		{name: "installed local hook", secondVisit: "local"},
		{name: "updated global session-changed hook", secondVisit: "global", secondHook: "client-session-changed"},
		{name: "installed local session-changed hook", secondVisit: "local", secondHook: "client-session-changed"},
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
			secondHook := tc.secondHook
			if secondHook == "" {
				secondHook = "client-attached"
			}
			if tc.secondVisit != "" {
				contents += "set -g destroy-unattached keep-last\n"
				if tc.secondVisit == "local" {
					contents += `set-hook -g ` + secondHook + `[5] { set-hook -t workspace ` + secondHook + `[0] { set -gF @followup-zero "#{hook_client}" ; set -gF @user-attach-client "#{hook_client}" }; set-hook -t workspace ` + secondHook + `[9] { set -gF @followup "#{hook_client}" }; set -g @installed yes }` + "\n"
				} else {
					contents += "set-hook -g " + secondHook + "[5] { set -g @installed yes }\n"
				}
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
				attachedSession := destination
				if tc.switchClient {
					attachedSession = "workspace"
				}
				clientName, err := command.RunCommandOutputContext(ctx, "list-clients", "-t", attachedSession, "-F", "#{client_name}")
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
			if tc.multiplePending {
				require.NoError(t, runner.Ensure(ctx, "spare", root, BlankLayout()))
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
				if tc.localHooks {
					require.NoError(t, command.KillSessionContext(ctx, "spare"))
				}
			}
			visits := 1
			if tc.secondVisit != "" {
				visits = 2
				policy, err := command.RunCommandOutputContext(ctx, "show-options", "-Av", "-t", "workspace", "destroy-unattached")
				require.NoError(t, err)
				require.Equal(t, "off", strings.TrimSpace(policy))
			}
			for visit := range visits {
				if visit == 1 && tc.secondVisit == "global" {
					require.NoError(t, command.RunCommandContext(ctx, "set-hook", "-g", secondHook, `set -g @user-attach-hook ran ; set -gF @user-attach-client "#{hook_client}" ; set -gF @seen "new:#{hook_client}"`))
				}
				if !tc.earlyAttach || visit > 0 {
					attach()
				}
				out, err := command.RunCommandOutputContext(ctx, "show-options", "-Av", "-t", "workspace", "destroy-unattached")
				require.NoError(t, err)
				expectedPolicy := "on"
				if tc.secondVisit != "" {
					expectedPolicy = "keep-last"
				}
				if tc.user999 && !tc.localHooks {
					expectedPolicy = "off" // User-owned slots take priority over restoration.
				}
				require.Equal(t, expectedPolicy, strings.TrimSpace(out))
				if !tc.user999 {
					for _, hook := range []string{"client-attached", "client-session-changed"} {
						out, err := command.RunCommandOutputContext(ctx, "show-hooks", "-g", hook)
						require.NoError(t, err)
						if tc.multiplePending {
							require.Contains(t, out, "@kwt-destroy-unattached", "another session still needs the restore hook")
						} else if !tc.localHooks && (tc.secondVisit != "local" || hook != secondHook) {
							require.NotContains(t, out, "[999]", "global hooks must leave with the last pending marker")
						}
					}
				} else if tc.localHooks {
					out, err := command.RunCommandOutputContext(ctx, "show-hooks", "-g")
					require.NoError(t, err)
					require.Equal(t, globalHooks, out, "cleanup must preserve user-owned global hooks")
				}
				if tc.secondVisit != "" {
					require.Eventually(t, func() bool {
						if visit == 0 {
							out, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", "@installed")
							return err == nil && strings.TrimSpace(out) == "yes"
						}
						clientName, err := command.RunCommandOutputContext(ctx, "list-clients", "-t", "workspace", "-F", "#{client_name}")
						if err != nil || strings.TrimSpace(clientName) == "" {
							return false
						}
						option, prefix := "@seen", "new:"
						if tc.secondVisit == "local" {
							option, prefix = "@followup", ""
							out, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", "@followup-zero")
							if err != nil || strings.TrimSpace(out) != strings.TrimSpace(clientName) {
								return false
							}
						}
						out, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", option)
						return err == nil && strings.TrimSpace(out) == prefix+strings.TrimSpace(clientName)
					}, 5*time.Second, 10*time.Millisecond)
				}
				marker := filepath.Join(root, fmt.Sprintf("shell-ready-%d", visit))
				_, err = fmt.Fprintf(terminal, "printf 'ready-%%s' worktree > %s\r", quotePOSIXShellArgument(marker))
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					contents, err := os.ReadFile(marker)
					return err == nil && string(contents) == "ready-worktree"
				}, 5*time.Second, 10*time.Millisecond)
				if tc.followGlobal {
					require.NoError(t, command.RunCommandContext(ctx, "set-option", "-g", "destroy-unattached", "off"))
				}
				// Disconnect the client directly. Tmux 3.6's detach-client path can
				// redraw a session after destroy-unattached removes its last window.
				require.NoError(t, client.Process.Kill())
				var exitErr *exec.ExitError
				require.ErrorAs(t, client.Wait(), &exitErr)
				require.Equal(t, -1, exitErr.ExitCode())
				if tc.followGlobal || tc.secondVisit != "" {
					require.True(t, command.HasSession("workspace"))
				} else if !tc.user999 || tc.localHooks {
					require.Eventually(t, func() bool { return !command.HasSession("workspace") }, 5*time.Second, 10*time.Millisecond)
				}
			}
			if tc.multiplePending {
				destination = "spare"
				attach()
				for _, hook := range []string{"client-attached", "client-session-changed"} {
					out, err := command.RunCommandOutputContext(ctx, "show-hooks", "-g", hook)
					require.NoError(t, err)
					require.NotContains(t, out, "[999]", "the last pending session must remove both global restore hooks")
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
