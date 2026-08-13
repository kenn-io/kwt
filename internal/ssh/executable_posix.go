//go:build !windows

package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func resolveExecutable(name string, environment []string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return name, nil
	}
	path := ""
	for index := len(environment) - 1; index >= 0; index-- {
		key, value, ok := strings.Cut(environment[index], "=")
		if ok && key == "PATH" {
			path = value
			break
		}
	}
	for _, directory := range filepath.SplitList(path) {
		if !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}
