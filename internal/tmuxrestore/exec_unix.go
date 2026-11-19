//go:build !windows

package tmuxrestore

import (
	"os"
	"syscall"
)

// Exec replaces the current process with target's program, in target's
// directory when it still exists. It returns only on failure.
func Exec(target ExecTarget) error {
	if len(target.Argv) == 0 || target.Path == "" {
		return os.ErrInvalid
	}
	if target.Dir != "" {
		_ = os.Chdir(target.Dir)
	}
	return syscall.Exec(target.Path, target.Argv, os.Environ())
}
