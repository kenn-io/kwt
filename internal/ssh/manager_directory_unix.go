//go:build unix

package ssh

import "os"

func newManagerDirectory() (string, error) {
	return os.MkdirTemp("/tmp", "kwt-ssh-")
}
