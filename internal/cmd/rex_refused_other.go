//go:build !windows

package cmd

import (
	"errors"
	"syscall"
)

// connectionRefused reports a server address with nothing listening.
func connectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
