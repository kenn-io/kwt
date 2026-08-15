//go:build windows

package ssh

import "os"

func newManagerDirectory() (string, error) {
	return os.MkdirTemp("", "kwt-ssh-")
}
