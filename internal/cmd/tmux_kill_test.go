package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fake executable is the only tmux on PATH, including for the canonical
// KWT socket, so this test cannot inspect or terminate a real session.
func TestTmuxKillConfirmationFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic tmux uses a POSIX shell")
	}
	for _, tc := range []struct {
		name, flag, input    string
		wantPrompt, wantKill bool
	}{
		{name: "default rejects", input: "n\n", wantPrompt: true},
		{name: "default confirms", input: "y\n", wantPrompt: true, wantKill: true},
		{name: "yes", flag: "--yes", wantKill: true},
		{name: "legacy force", flag: "--force", wantKill: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, calls, err := runSyntheticTmuxKill(t, tc.input, false, tc.flag)
			require.NoError(t, err, output)
			require.Equal(t, tc.wantPrompt, strings.Contains(output, "Are you sure?"), output)
			require.Equal(t, tc.wantKill, strings.Contains(calls, "kill-session"), calls)
		})
	}
}

func TestTmuxKillYesPreservesInteractiveSelection(t *testing.T) {
	output, calls, err := runSyntheticTmuxKill(t, "", true, "--yes")
	// Without a terminal the finder fails, demonstrating that multiple matches
	// still go through selection instead of being terminated automatically.
	require.Error(t, err, output)
	require.Contains(t, output, "session selection cancelled")
	require.NotContains(t, calls, "kill-session")
}

func runSyntheticTmuxKill(t *testing.T, input string, multiple bool, flag string) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic tmux uses a POSIX shell")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KWT_FAKE_LOG"
if [ "$1" = "-L" ]; then
  shift 2
else
  exit 0
fi
case "$1" in
  list-sessions)
    printf 'kwt-wt-work-a1b2c3d4:1:1:0:sh:/tmp\n'
    if [ "$KWT_FAKE_MULTIPLE" = 1 ]; then
      printf 'kwt-wt-work-e5f6a7b8:1:1:0:sh:/tmp\n'
    fi
    ;;
  has-session|kill-session) exit 0 ;;
  *) exit 1 ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700))
	args := []string{"tmux", "kill", "work"}
	if flag != "" {
		args = append(args, flag)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestKwtHelperProcess$")
	cmd.Env = append(os.Environ(), "PATH="+dir, "KWT_HOME="+filepath.Join(dir, "home"),
		"KWT_FAKE_LOG="+logPath, "KWT_TEST_ARGS="+strings.Join(args, "\n"))
	if multiple {
		cmd.Env = append(cmd.Env, "KWT_FAKE_MULTIPLE=1")
	}
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	calls, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return string(output), string(calls), err
}
