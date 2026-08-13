//go:build !windows

package ssh

import (
	"net"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/safefileio"
)

const maximumAskpassSocketPathBytes = 100

func listenAskpass(
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
	listener, err := net.Listen("unix", path)
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

func dialAskpass(handle askpassHandle, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(handle.Network, handle.Address, timeout)
}
