//go:build !windows

package tmuxrestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

var currentUID = func() uint32 { return uint32(os.Geteuid()) }

// checkSocketOwner refuses a tmux socket that another account could put a
// server behind. tmux checks its socket directory when it picks the socket
// itself, but not for the explicit paths kwt passes, and a server controlled
// by someone else could read saved conversations or plant a launch to
// replay. The directory must belong to this account or to root, and a socket
// already there must belong to this account.
func checkSocketOwner(path string) error {
	uid := currentUID()
	owner := func(info fs.FileInfo) (uint32, bool) {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return stat.Uid, true
	}
	dir, err := os.Stat(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect tmux socket directory: %w", err)
	}
	if got, ok := owner(dir); ok && got != uid && got != 0 {
		return fmt.Errorf("tmux socket directory %s belongs to another account", filepath.Dir(path))
	}
	socket, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect tmux socket: %w", err)
	}
	if got, ok := owner(socket); ok && got != uid {
		return fmt.Errorf("tmux socket %s belongs to another account", path)
	}
	return nil
}
