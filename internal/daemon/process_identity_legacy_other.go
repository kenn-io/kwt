//go:build !linux

package daemon

func readLegacyLinuxProcessIdentity(int) (string, bool) {
	return "", false
}

func legacyLinuxProcessIdentityCompatible(string) bool {
	return false
}
