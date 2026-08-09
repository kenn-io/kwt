//go:build !windows

package kwt

import "os"

func replaceCacheFile(source, destination string) error {
	return os.Rename(source, destination)
}
