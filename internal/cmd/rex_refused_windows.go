//go:build windows

package cmd

import (
	"errors"

	"golang.org/x/sys/windows"
)

// connectionRefused reports a server address with nothing listening.
// Winsock reports that with its own error code, not ECONNREFUSED.
func connectionRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
