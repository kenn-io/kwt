//go:build windows

package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func resolveExecutable(name string, environment []string, workingDirectory string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	if strings.ContainsAny(name, `:\/`) {
		return filepath.Join(workingDirectory, name), nil
	}
	path := windowsEnvironmentValue(environment, "PATH")
	extensions := windowsExecutableExtensions(
		windowsEnvironmentValue(environment, "PATHEXT"),
	)
	for _, directory := range filepath.SplitList(path) {
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(workingDirectory, directory)
		}
		if !filepath.IsAbs(directory) {
			continue
		}
		base := filepath.Join(directory, name)
		candidates := make([]string, 0, len(extensions)+1)
		if filepath.Ext(name) != "" {
			candidates = append(candidates, base)
		}
		for _, extension := range extensions {
			candidates = append(candidates, base+extension)
		}
		for _, candidate := range candidates {
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func windowsEnvironmentValue(environment []string, name string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		key, value, ok := strings.Cut(environment[index], "=")
		if ok && strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func windowsExecutableExtensions(value string) []string {
	if value == "" {
		value = ".COM;.EXE;.BAT;.CMD"
	}
	extensions := make([]string, 0, 4)
	for _, extension := range strings.Split(value, ";") {
		if extension == "" {
			continue
		}
		if extension[0] != '.' {
			extension = "." + extension
		}
		extensions = append(extensions, extension)
	}
	return extensions
}
