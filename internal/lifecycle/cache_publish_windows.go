//go:build windows

package lifecycle

import "golang.org/x/sys/windows"

func replaceCacheFile(source, destination string) error {
	return windows.Rename(source, destination)
}
