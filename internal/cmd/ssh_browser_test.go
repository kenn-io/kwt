package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
)

func TestSSHBrowserPrompt(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		json, open, fails, short bool
	}{
		{name: "short headless", short: true}, {name: "short opens", short: true, open: true}, {name: "defer"}, {name: "open", open: true}, {name: "opener unavailable", open: true, fails: true},
		{name: "JSON defers", json: true}, {name: "JSON explicitly opens", json: true, open: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldAcquire, oldOpen, oldResolve := acquireSSHLeaseThroughDaemon, openSSHBrowser, resolveSSHThroughDaemon
			oldJSON, oldIdentity, oldBrowser := sshLeaseJSON, sshLeaseRouteIdentity, sshOpenBrowser
			t.Cleanup(func() {
				acquireSSHLeaseThroughDaemon, openSSHBrowser, resolveSSHThroughDaemon = oldAcquire, oldOpen, oldResolve
				sshLeaseJSON, sshLeaseRouteIdentity, sshOpenBrowser = oldJSON, oldIdentity, oldBrowser
			})
			sshLeaseJSON, sshLeaseRouteIdentity, sshOpenBrowser = tc.json, "route-one", tc.open
			resolveSSHThroughDaemon = func(context.Context, kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error) {
				return kwt.SSHRouteSnapshot{}, nil
			}
			opened := false
			openSSHBrowser = func(_ context.Context, url string) error {
				opened = true
				assert.Equal(t, "https://login.tailscale.com/a/example", url)
				if tc.fails {
					return errors.New("no desktop")
				}
				return nil
			}
			acquireSSHLeaseThroughDaemon = func(ctx context.Context, _ kwt.SSHLeaseRequest, callbacks kwtdaemon.OperationCallbacks) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
				p := service.OperationPrompt{ID: "browser", Kind: "ssh_browser_authentication", Message: "To authenticate, visit: https://login.tailscale.com/a/example", Details: map[string]any{"authentication_url": "https://login.tailscale.com/a/example"}}
				require.NoError(t, callbacks.Event(service.OperationEvent{Kind: service.OperationEventPrompt, Prompt: &p}))
				answer, err := callbacks.Prompt(ctx, p)
				require.NoError(t, err)
				assert.Empty(t, answer)
				return kwtdaemon.SSHLeaseResult{}, nil, service.NewError(service.SSHPromptRejected, "end of fixture", false, nil, nil)
			}
			command, stdout, stderr := sshResolveTestCommand()
			input := ""
			if tc.json {
				input = `{"prompt_id":"browser","value":""}`
			}
			command.SetIn(strings.NewReader(input))
			if tc.short {
				_, _, _, err := acquireShortSSHLease(command, kwt.SSHTarget{Hostname: "build.example.test"}, kwt.SSHHostKeyPolicy("review"), "", false)
				require.Error(t, err)
			} else {
				require.Error(t, runSSHLease(command, []string{"build.example.test"}))
			}
			assert.Equal(t, tc.open, opened)
			if tc.json {
				assert.Contains(t, stdout.String(), "https://login.tailscale.com/a/example")
			} else {
				assert.Contains(t, stderr.String(), "https://login.tailscale.com/a/example")
			}
			if tc.fails {
				assert.Contains(t, stderr.String(), "Open the URL manually")
			}
		})
	}
}

func TestSSHBrowserDesktopSelection(t *testing.T) {
	for _, tc := range []struct {
		name, platform string
		env            map[string]string
		want           []string
	}{
		{name: "Mac desktop", platform: "darwin", want: []string{"open"}},
		{name: "Mac over SSH", platform: "darwin", env: map[string]string{"SSH_CONNECTION": "remote"}},
		{name: "Linux without display", platform: "linux"},
		{name: "Wayland", platform: "linux", env: map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, want: []string{"xdg-open"}},
		{name: "X11", platform: "linux", env: map[string]string{"DISPLAY": ":0"}, want: []string{"xdg-open"}},
		{name: "Windows desktop", platform: "windows", want: []string{"rundll32", "url.dll,FileProtocolHandler"}},
		{name: "Windows service", platform: "windows", env: map[string]string{"SESSIONNAME": "Services"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sshBrowserCommand(tc.platform, func(k string) string { return tc.env[k] }))
		})
	}
}
