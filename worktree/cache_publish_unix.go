//go:build !windows

package worktree

import "os"

func replaceCacheFile(source, destination string) error {
	return os.Rename(source, destination)
}
