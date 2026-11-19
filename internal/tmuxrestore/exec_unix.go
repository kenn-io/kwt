//go:build !windows

package tmuxrestore

import (
	"os"
	"os/exec"
	"syscall"
)

// Exec replaces the current process with target's program, in target's
// directory when it still exists. It returns only on failure.
func Exec(target ExecTarget) error {
	if len(target.Argv) == 0 {
		return os.ErrInvalid
	}
	if target.Dir != "" {
		_ = os.Chdir(target.Dir)
	}
	path, err := exec.LookPath(target.Argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, target.Argv, os.Environ())
}
