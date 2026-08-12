//go:build !linux

package daemon

func readStableProcessIdentity(int) (string, bool) {
	return "", false
}
