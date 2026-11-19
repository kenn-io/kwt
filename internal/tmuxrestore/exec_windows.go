//go:build windows

package tmuxrestore

import "errors"

// Exec is unavailable on Windows, which has no tmux panes to restore into.
func Exec(ExecTarget) error {
	return errors.New("restored programs start only on POSIX hosts")
}
