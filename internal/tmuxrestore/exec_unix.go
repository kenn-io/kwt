//go:build !windows

package tmuxrestore

import (
	"os"
	"syscall"
)

// Exec replaces the current process with target's program, in target's
// directory. It returns only on failure, including when the directory is
// gone, so the program never starts somewhere else.
func Exec(target ExecTarget) error {
	if len(target.Argv) == 0 || target.Path == "" {
		return os.ErrInvalid
	}
	if target.Dir != "" {
		if err := os.Chdir(target.Dir); err != nil {
			return err
		}
	}
	return syscall.Exec(target.Path, target.Argv, os.Environ())
}
