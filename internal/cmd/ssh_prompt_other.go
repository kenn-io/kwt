//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package cmd

import (
	"errors"
	"os"
)

func newTerminalPasswordReader(*os.File) (terminalPasswordReader, error) {
	return nil, errors.New("interruptible terminal password input is unsupported on this platform")
}
