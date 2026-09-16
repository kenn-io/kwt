//go:build !windows

package ssh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
)

func TestCompressionOverridesDestinationAcrossLeaseRevalidation(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH is unavailable")
	}
	for _, configured := range []string{"yes", "no"} {
		t.Run(configured, func(t *testing.T) {
			directory := t.TempDir()
			configPath := filepath.Join(directory, "config")
			require.NoError(t, os.WriteFile(configPath, []byte("Host compression.example.test\n  ProxyJump relay.example.test\nHost *\n  User deploy\n  Compression "+configured+"\n"), 0o600))
			executable := filepath.Join(directory, "ssh")
			require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexec "+shellQuote(ssh)+" -F "+shellQuote(configPath)+" \"$@\"\n"), 0o700))
			persistent := &fakePersistentManager{generation: 1}
			manager := NewManager(ManagerOptions{
				Persistent: persistent,
				Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
					return func(context.Context, []string) (int, error) { return 0, nil }, nil
				},
			})
			t.Cleanup(func() { require.NoError(t, manager.Close(context.Background())) })
			identities := make(map[string]string)
			masters := make(map[string]string)
			for _, override := range []struct {
				name  string
				value *bool
			}{
				{name: "config"},
				{name: "yes", value: new(true)},
				{name: "no", value: new(false)},
			} {
				t.Run(override.name, func(t *testing.T) {
					expected := override.name
					if override.value == nil {
						expected = configured
					}
					resolver := NewResolver(ResolverOptions{
						Executable:  executable,
						LoginShell:  func() (string, error) { return "/bin/sh", nil },
						Nonce:       func() (string, error) { return "compression-test", nil },
						Environment: []string{"HOME=" + directory, "PATH=/usr/bin:/bin"},
						Run: func(ctx context.Context, argv []string, cwd string, environment []string, input []byte) ([]byte, []byte, int, error) {
							stdout, stderr, code, runErr := runOutput(ctx, argv, cwd, environment, input)
							if runErr == nil {
								output, frameErr := framedOutput(stdout, "KWT_SSH_CONFIG_START_compression-test", "KWT_SSH_CONFIG_END_compression-test")
								require.NoError(t, frameErr)
								want := expected
								if strings.Contains(resolveCommandFromEnvironment(t, environment), "'relay.example.test'") {
									want = configured
								}
								assert.Contains(t, openssh.ParseConfig(output).Options, openssh.Option{Name: "compression", Value: want})
							}
							return stdout, stderr, code, runErr
						},
					})
					service := NewService(ServiceOptions{Resolver: resolver, Leases: manager})
					snapshot, err := service.Resolve(t.Context(), ResolveRequest{
						Target: Target{Hostname: "compression.example.test"}, Compression: override.value,
					})
					require.NoError(t, err)
					assert.Equal(t, override.value, snapshot.Compression)
					require.Len(t, snapshot.Targets, 2)
					assert.Contains(t, snapshot.Targets[0].Projection.Arguments, "Compression="+configured)
					assert.Contains(t, snapshot.Targets[1].Projection.Arguments, "Compression="+expected)
					identities[override.name] = snapshot.RouteIdentity
					lease, err := service.Acquire(t.Context(), LeaseRequest{Snapshot: snapshot})
					require.NoError(t, err)
					require.NoError(t, lease.Release(t.Context()))
					public := NewPublicService(PublicServiceOptions{Home: directory})
					public.build = func(ResolverOptions) snapshotResolver { return service }
					public.leases = manager
					lease, err = public.Acquire(t.Context(), LeaseRequest{Snapshot: snapshot})
					require.NoError(t, err)
					require.NoError(t, lease.Release(t.Context()))
					masters[override.name] = persistent.connectIdentities[len(persistent.connectIdentities)-1]
				})
			}
			assert.NotEqual(t, identities["yes"], identities["no"])
			assert.Equal(t, identities[configured], identities["config"])
			assert.NotEqual(t, masters["yes"], masters["no"])
			assert.Equal(t, masters[configured], masters["config"])
		})
	}
}

func TestProjectionPreservesCompression(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH is unavailable")
	}
	for _, compression := range []string{"yes", "no"} {
		t.Run(compression, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(configPath, []byte("Host compression.example.test\n  Compression "+compression+"\n"), 0o600))
			output, err := exec.CommandContext(t.Context(), ssh, "-G", "-F", configPath, "compression.example.test").Output()
			require.NoError(t, err)
			projected, err := projectConfig(openssh.ParseConfig(output), []string{})
			require.NoError(t, err)
			args := append(projected.Arguments, "-G", "compression.example.test")
			output, err = exec.CommandContext(t.Context(), ssh, args...).Output()
			require.NoError(t, err)
			var replayedCompression string
			for _, option := range openssh.ParseConfig(output).Options {
				if option.Name == "compression" {
					replayedCompression = option.Value
				}
			}
			require.Equal(t, compression, replayedCompression)
		})
	}
}
