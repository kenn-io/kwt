package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A workspace shell must cross the credential boundary before login startup.
func TestHerdrWorkspaceHelperSanitizesLoginShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	root := t.TempDir()
	shell := filepath.Join(root, "fixture-shell")
	result := filepath.Join(root, "shell-result")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FIXTURE_RESULT\"\n/usr/bin/env >> \"$FIXTURE_RESULT\"\n"
	if err := os.WriteFile(shell, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	t.Setenv("FIXTURE_RESULT", result)
	t.Setenv("KWT_GITHUB_TOKEN", "synthetic-secret")
	t.Setenv("FIXTURE_CUSTOM_TOKEN", "synthetic-custom")
	command := newHerdrHelperCommand()
	t.Chdir(root)
	command.SetArgs([]string{"--workspace", "--workspace-path", root, "--workspace-generation", "", "--strip", "FIXTURE_CUSTOM_TOKEN", "--", ""})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if !strings.Contains(text, "-l\n") || strings.Contains(text, "synthetic-secret") || strings.Contains(text, "synthetic-custom") {
		t.Fatalf("workspace did not enter sanitized login shell: %s", text)
	}
}
