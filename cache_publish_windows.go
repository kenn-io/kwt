//go:build windows

package kwt

import "golang.org/x/sys/windows"

func replaceCacheFile(source, destination string) error {
	return windows.Rename(source, destination)
}
