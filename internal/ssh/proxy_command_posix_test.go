//go:build !windows

package ssh

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
)

func TestTargetProxyCommandWinsOpenSSHEffectiveConfiguration(t *testing.T) {
	previous := ResolvedTarget{
		EffectiveTarget: Target{User: "relay", Hostname: "relay.example.test", Port: 2222},
	}
	target := targetWithProxy(
		ResolvedTarget{
			EffectiveTarget: Target{User: "deploy", Hostname: "build.example.test", Port: 22},
			Projection: ExecutionProjection{Arguments: []string{
				"-F", os.DevNull,
				"-o", "HostName=build.example.test",
			}},
		},
		previous,
		[]string{"-S", filepath.Join(t.TempDir(), "control")},
	)

	arguments := append([]string{"-G"}, target.Projection.Arguments...)
	arguments = append(arguments, "--", "build.example.test")
	output, err := exec.Command("ssh", arguments...).CombinedOutput()
	require.NoError(t, err, string(output))
	config := openssh.ParseConfig(output)
	proxyCommandValue := ""
	for _, option := range config.Options {
		if option.Name == "proxycommand" {
			proxyCommandValue = option.Value
			break
		}
	}
	assert.Contains(t, proxyCommandValue, "-S")
	assert.Contains(t, proxyCommandValue, "relay@relay.example.test")
}

func TestProxyCommandDoesNotConnectDirectlyWhenControlSocketIsMissing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { assert.NoError(t, listener.Close()) }()
	port := listener.Addr().(*net.TCPAddr).Port
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
			close(accepted)
		}
	}()

	commandText := proxyCommand(
		Target{User: "relay", Hostname: "127.0.0.1", Port: port},
		[]string{"-S", filepath.Join(t.TempDir(), "missing-control")},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", commandText)
	err = command.Run()
	require.Error(t, err)
	assert.NotEqual(t, context.DeadlineExceeded, ctx.Err())

	select {
	case <-accepted:
		t.Fatal("proxy transport connected directly after the control socket was absent")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestMultiplexedLeaseDoesNotConnectDirectlyWhenControlSocketIsMissing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { assert.NoError(t, listener.Close()) }()
	port := listener.Addr().(*net.TCPAddr).Port
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
			close(accepted)
		}
	}()

	persistent := &fakePersistentManager{
		generation: 1,
		connectionArguments: []string{
			"-o", "ControlMaster=no",
			"-o", "ControlPersist=no",
			"-S", filepath.Join(
				os.TempDir(), "kwt-missing-"+strconv.FormatInt(time.Now().UnixNano(), 10),
			),
		},
	}
	manager := NewManager(ManagerOptions{
		Persistent: persistent,
		Runner: func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error) {
			return func(context.Context, []string) (int, error) { return 0, nil }, nil
		},
	})
	snapshot := directSnapshot("fail-closed-route")
	lease, err := manager.Acquire(
		context.Background(),
		LeaseRequest{Snapshot: snapshot},
		func(context.Context) (RouteSnapshot, error) { return snapshot, nil },
	)
	require.NoError(t, err)
	defer func() { assert.NoError(t, lease.Release(context.Background())) }()
	arguments, err := lease.Arguments(context.Background())
	require.NoError(t, err)
	arguments = append(arguments,
		"-o", "ConnectTimeout=1",
		"-p", strconv.Itoa(port),
		"--", "127.0.0.1",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "ssh", arguments...)
	err = command.Run()
	require.Error(t, err)
	assert.NotEqual(t, context.DeadlineExceeded, ctx.Err())

	select {
	case <-accepted:
		t.Fatal("multiplexed lease connected directly after the control socket was absent")
	case <-time.After(100 * time.Millisecond):
	}
}
