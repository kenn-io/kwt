//go:build windows

package worktree

import "golang.org/x/sys/windows"

func replaceCacheFile(source, destination string) error {
	return windows.Rename(source, destination)
}
