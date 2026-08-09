//go:build !windows

package lifecycle

import "os"

func replaceCacheFile(source, destination string) error {
	return os.Rename(source, destination)
}
