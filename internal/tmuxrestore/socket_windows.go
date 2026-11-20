//go:build windows

package tmuxrestore

// checkSocketOwner has nothing to check on Windows, which has no tmux.
func checkSocketOwner(string) error { return nil }
