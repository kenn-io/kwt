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
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	socket := fmt.Sprintf("kwt-lifetime-off-%d-%d", os.Getpid(), time.Now().UnixNano())
	command := NewTmuxCommandForSocketWithStripNames("tmux", socket, nil)
	t.Cleanup(func() { killPrivateTmuxServer(socket) })
	require.NoError(t, command.RunCommandContext(ctx, "-f", "/dev/null", "new-session", "-d", "-s", "unrelated"))
	require.NoError(t, command.RunCommandContext(ctx, "set-option", "-g", "destroy-unattached", "off"))
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
	require.NotContains(t, output, "[999]")
}

func TestWorkspaceRestoresGlobalHookInheritance(t *testing.T) {
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	for _, hook := range []string{"client-attached", "client-session-changed"} {
		t.Run(hook, func(t *testing.T) {
			for _, tc := range []struct {
				name, command, replacement, option, prefix, zeroOption string
			}{
				{name: "updated global hook", command: "set -g @installed yes", replacement: `set -gF @seen "new:#{hook_client}"`, option: "@seen", prefix: "new:"},
				{name: "installed local hook", command: "set-hook -t workspace " + hook + `[0] { set -gF @followup-zero "#{hook_client}" }; set-hook -t workspace ` + hook + `[9] { set -gF @followup "#{hook_client}" }; set -g @installed yes`, option: "@followup", zeroOption: "@followup-zero"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
					defer cancel()
					root := t.TempDir()
					config := filepath.Join(root, "tmux.conf")
					contents := "set -g destroy-unattached keep-last\nset -g default-shell /bin/sh\nset-hook -g " + hook + " { " + tc.command + " }\n"
					require.NoError(t, os.WriteFile(config, []byte(contents), 0600))
					wrapper := filepath.Join(root, "tmux")
					require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quotePOSIXShellArgument(binary)+" -f "+quotePOSIXShellArgument(config)+" \"$@\"\n"), 0700))
					socket := fmt.Sprintf("kwt-lifetime-reload-%d-%d", os.Getpid(), time.Now().UnixNano())
					command := NewTmuxCommandForSocketWithStripNames(wrapper, socket, nil)
					t.Cleanup(func() { killPrivateTmuxServer(socket) })
					runner := NewWorkspaceRunner(command, nil)
					require.NoError(t, runner.Ensure(ctx, "workspace", root, BlankLayout()))
					for visit := range 2 {
						if visit == 1 && tc.replacement != "" {
							require.NoError(t, command.RunCommandContext(ctx, "set-hook", "-g", hook, tc.replacement))
						}
						client := command.attachSessionCmd("workspace")
						client.Env = append(client.Env, "TERM=xterm-256color")
						terminal, err := pty.Start(client)
						require.NoError(t, err)
						drained := make(chan struct{})
						t.Cleanup(func() { _ = terminal.Close(); _ = client.Process.Kill(); _ = client.Wait(); <-drained })
						go func() { defer close(drained); _, _ = io.Copy(io.Discard, terminal) }()
						require.Eventually(t, func() bool {
							if visit == 0 {
								output, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", "@installed")
								return err == nil && strings.TrimSpace(output) == "yes"
							}
							clientName, err := command.RunCommandOutputContext(ctx, "list-clients", "-t", "workspace", "-F", "#{client_name}")
							if err != nil || strings.TrimSpace(clientName) == "" {
								return false
							}
							output, err := command.RunCommandOutputContext(ctx, "show-options", "-gqv", tc.option)
							if err != nil || strings.TrimSpace(output) != tc.prefix+strings.TrimSpace(clientName) {
								return false
							}
							if tc.zeroOption == "" {
								return true
							}
							output, err = command.RunCommandOutputContext(ctx, "show-options", "-gqv", tc.zeroOption)
							return err == nil && strings.TrimSpace(output) == strings.TrimSpace(clientName)
						}, 5*time.Second, 10*time.Millisecond)
						require.NoError(t, command.RunCommandContext(ctx, "detach-client", "-s", "workspace"))
						require.NoError(t, client.Wait())
					}
				})
			}
		})
	}
}

func TestWorkspaceSurvivesBootstrapWithDestroyUnattached(t *testing.T) {
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	for _, tc := range []struct {
		name                                                      string
		warm, switchClient, localHooks, earlyAttach, followGlobal bool
	}{
		{name: "fresh server"},
		{name: "existing server", warm: true},
		{name: "existing session hooks", localHooks: true},
		{name: "switch client", warm: true, switchClient: true},
		{name: "attach before hook installation", earlyAttach: true},
		{name: "switch before hook installation", warm: true, switchClient: true, earlyAttach: true},
		{name: "global policy change", followGlobal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			root := t.TempDir()
			config := filepath.Join(root, "tmux.conf")
			contents := "set -g destroy-unattached on\nset -g default-shell /bin/sh\nset-hook -g client-attached 'set -g @user-attach-hook ran'\nset-hook -g client-session-changed 'set -g @user-switch-hook ran'\n" +
				"set-hook -g client-attached[3] 'set -gF @user-attach-client \"#{hook_client}\"'\n" +
				"set-hook -g client-session-changed[3] 'set -gF @user-switch-client \"#{hook_client}\"'\n"
			if tc.localHooks {
				contents += "set-hook -g after-new-session { set-hook client-attached[4] { set -g @user-attach-hook local }; set-hook client-session-changed[7] { set -g @user-switch-hook local }; set-hook client-attached[9] { set -gF @user-attach-client \"#{hook_client}\" }; set-hook client-session-changed[9] { set -gF @user-switch-client \"#{hook_client}\" } }\n"
			}
			require.NoError(t, os.WriteFile(config, []byte(contents), 0600))
			wrapper := filepath.Join(root, "tmux")
			require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quotePOSIXShellArgument(binary)+" -f "+quotePOSIXShellArgument(config)+" \"$@\"\n"), 0700))
			socket := fmt.Sprintf("kwt-lifetime-%d-%d", os.Getpid(), time.Now().UnixNano())
			command := NewTmuxCommandForSocketWithStripNames(wrapper, socket, nil)
			t.Cleanup(func() { killPrivateTmuxServer(socket) })
			if tc.warm {
				require.NoError(t, command.RunCommandContext(ctx, "new-session", "-d", "-s", "unrelated", "sleep", "30", ";", "set-option", "-t", "unrelated", "destroy-unattached", "off"))
			}
			var terminal *os.File
			destination := "workspace"
			if tc.switchClient {
				destination = "unrelated"
			}
			attach := func() {
				client := command.attachSessionCmd(destination)
				client.Env = append(client.Env, "TERM=xterm-256color")
				var err error
				terminal, err = pty.Start(client)
				require.NoError(t, err)
				drained := make(chan struct{})
				t.Cleanup(func() { _ = terminal.Close(); _ = client.Process.Kill(); _ = client.Wait(); <-drained })
				go func() { defer close(drained); _, _ = io.Copy(io.Discard, terminal) }()
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
			if !tc.earlyAttach {
				attach()
			}
			out, err := command.RunCommandOutputContext(ctx, "show-options", "-Av", "-t", "workspace", "destroy-unattached")
			require.NoError(t, err)
			require.Equal(t, "on", strings.TrimSpace(out))
			marker := filepath.Join(root, "shell-ready")
			_, err = fmt.Fprintf(terminal, "printf 'ready-%%s' worktree > %s\r", quotePOSIXShellArgument(marker))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				contents, err := os.ReadFile(marker)
				return err == nil && string(contents) == "ready-worktree"
			}, 5*time.Second, 10*time.Millisecond)
			if tc.followGlobal {
				require.NoError(t, command.RunCommandContext(ctx, "set-option", "-g", "destroy-unattached", "off"))
			}
			require.NoError(t, command.RunCommandContext(ctx, "detach-client", "-s", "workspace"))
			if tc.followGlobal {
				require.True(t, command.HasSession("workspace"))
			} else {
				require.Eventually(t, func() bool { return !command.HasSession("workspace") }, 5*time.Second, 10*time.Millisecond)
			}
			if tc.warm {
				require.True(t, command.HasSession("unrelated"))
			}
		})
	}
}
