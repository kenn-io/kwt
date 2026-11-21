//go:build !windows

package cmd

import (
	"os"

	"golang.org/x/sys/unix"
)

func openBrowseDirectory(path string) (*os.File, error) {
	// The kernel checks the type at open time, including after a concurrent
	// replacement. Nonblocking also prevents special files from waiting.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
