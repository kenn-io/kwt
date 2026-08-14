//go:build windows

package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"go.kenn.io/kwt/service"
	"golang.org/x/sys/windows"
)

func listenAskpass(
	string,
) (net.Listener, askpassEndpoint, func() error, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, askpassEndpoint{}, nil, err
	}
	userSID, err := currentUserSID()
	if err != nil {
		return nil, askpassEndpoint{}, nil, err
	}
	path := `\\.\pipe\kwt-askpass-` + hex.EncodeToString(random[:])
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + userSID + ")",
		InputBufferSize:    service.MaxOperationMessageBytes + maxAskpassHandleBytes,
		OutputBufferSize:   service.MaxOperationResponseBytes + 5,
	})
	if err != nil {
		return nil, askpassEndpoint{}, nil, err
	}
	return listener, askpassEndpoint{
		Network: "npipe",
		Address: path,
	}, func() error { return nil }, nil
}

func currentUserSID() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close() //nolint:errcheck // Read-only token cleanup.
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	if user.User.Sid == nil {
		return "", fmt.Errorf("current process token has no user SID")
	}
	return user.User.Sid.String(), nil
}

func dialAskpass(handle askpassHandle, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return winio.DialPipeContext(ctx, handle.Address)
}
