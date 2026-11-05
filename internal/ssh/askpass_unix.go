//go:build !windows

package ssh

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/safefileio"
)

const maximumAskpassSocketPathBytes = 100

func listenAskpass(
	ctx context.Context,
	directory string,
) (net.Listener, askpassEndpoint, func() error, error) {
	path := filepath.Join(directory, "prompt.sock")
	cleanup := func() error { return nil }
	if len(path) > maximumAskpassSocketPathBytes {
		shortDirectory, err := os.MkdirTemp("", "kwt-ap-")
		if err != nil {
			return nil, askpassEndpoint{}, nil, err
		}
		if err := safefileio.EnsurePrivateDir(shortDirectory); err != nil {
			_ = os.RemoveAll(shortDirectory)
			return nil, askpassEndpoint{}, nil, err
		}
		path = filepath.Join(shortDirectory, "prompt.sock")
		cleanup = func() error { return os.RemoveAll(shortDirectory) }
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		_ = cleanup()
		return nil, askpassEndpoint{}, nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = cleanup()
		return nil, askpassEndpoint{}, nil, err
	}
	return listener, askpassEndpoint{Network: "unix", Address: path}, cleanup, nil
}

func dialAskpass(ctx context.Context, handle askpassHandle, timeout time.Duration) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout}).DialContext(ctx, handle.Network, handle.Address)
}
