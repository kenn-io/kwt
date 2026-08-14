//go:build linux

package cmd

import (
	"os"

	"golang.org/x/sys/unix"
)

func getTerminalState(terminal *os.File) (*unix.Termios, error) {
	return unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
}

func setTerminalState(terminal *os.File, state *unix.Termios) error {
	return unix.IoctlSetTermios(int(terminal.Fd()), unix.TCSETS, state)
}
