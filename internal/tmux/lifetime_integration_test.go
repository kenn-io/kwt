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

func TestWorkspaceSurvivesBootstrapWithDestroyUnattached(t *testing.T) {
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	for _, tc := range []struct {
		name                           string
		warm, switchClient, localHooks bool
	}{
		{name: "fresh server"},
		{name: "existing server", warm: true},
		{name: "existing session hooks", localHooks: true},
		{name: "switch client", warm: true, switchClient: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			root := t.TempDir()
			config := filepath.Join(root, "tmux.conf")
			contents := "set -g destroy-unattached on\nset -g default-shell /bin/sh\nset-hook -g client-attached 'set -g @user-attach-hook ran'\nset-hook -g client-session-changed 'set -g @user-switch-hook ran'\n"
			if tc.localHooks {
				contents += "set-hook -g after-new-session { set-hook client-attached[4] { set -g @user-attach-hook local }; set-hook client-session-changed[7] { set -g @user-switch-hook local } }\n"
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
			runner := NewWorkspaceRunner(command, nil)
			require.NoError(t, runner.Ensure(ctx, "workspace", root, BlankLayout()))
			destination := "workspace"
			if tc.switchClient {
				destination = "unrelated"
			}
			client := command.attachSessionCmd(destination)
			client.Env = append(client.Env, "TERM=xterm-256color")
			terminal, err := pty.Start(client)
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
			require.NoError(t, command.RunCommandContext(ctx, "detach-client", "-s", "workspace"))
			require.Eventually(t, func() bool { return !command.HasSession("workspace") }, 5*time.Second, 10*time.Millisecond)
			if tc.warm {
				require.True(t, command.HasSession("unrelated"))
			}
		})
	}
}
