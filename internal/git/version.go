package git

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var gitVersionPattern = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

// RequireVersion verifies that the installed Git is at least major.minor.
func RequireVersion(major, minor int) error {
	output, err := New("").RunCommand("version")
	if err != nil {
		return fmt.Errorf("determine Git version: %w", err)
	}
	return requireVersionOutput(output, major, minor)
}

func requireVersionOutput(output string, major, minor int) error {
	trimmed := strings.TrimSpace(output)
	match := gitVersionPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return fmt.Errorf("unexpected Git version output %q", trimmed)
	}

	installedMajor, err := strconv.Atoi(match[1])
	if err != nil {
		return fmt.Errorf("parse Git major version %q: %w", match[1], err)
	}
	installedMinor, err := strconv.Atoi(match[2])
	if err != nil {
		return fmt.Errorf("parse Git minor version %q: %w", match[2], err)
	}
	installedPatch := 0
	if match[3] != "" {
		installedPatch, err = strconv.Atoi(match[3])
		if err != nil {
			return fmt.Errorf("parse Git patch version %q: %w", match[3], err)
		}
	}

	if installedMajor < major || (installedMajor == major && installedMinor < minor) {
		return fmt.Errorf(
			"Git %d.%d or newer is required; found Git %d.%d.%d",
			major,
			minor,
			installedMajor,
			installedMinor,
			installedPatch,
		)
	}
	return nil
}
