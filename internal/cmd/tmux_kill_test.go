package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fake executable is the only tmux on PATH, including for the canonical
// KWT socket, so this test cannot inspect or terminate a real session.
func TestTmuxKillConfirmationFlags(t *testing.T) {
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
			output, calls, err := runSyntheticTmuxKill(t, tc.input, "a1b2c3d4", tc.flag)
			require.NoError(t, err, output)
			require.Equal(t, tc.wantPrompt, strings.Contains(output, "Are you sure?"), output)
			wantCalls := ""
			if tc.wantKill {
				wantCalls = "kwt-wt-work-a1b2c3d4\n"
			}
			require.Equal(t, wantCalls, calls)
		})
	}
}

func TestTmuxKillYesPreservesInteractiveSelection(t *testing.T) {
	output, calls, err := runSyntheticTmuxKill(t, "", "work", "--yes")
	// An unset TERM makes finder initialization fail even with a controlling TTY.
	require.Error(t, err, output)
	require.Contains(t, output, "session selection cancelled")
	require.Empty(t, calls)
}

func runSyntheticTmuxKill(t *testing.T, input, pattern, flag string) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic tmux uses a POSIX shell")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
if [ "$1" = kill-session ] || [ "$3" = kill-session ]; then
  for arg do target=$arg; done
  printf '%s\n' "${target#=}" >> "$KWT_FAKE_LOG"
fi
if [ "$1" = "-L" ]; then
  shift 2
else
  exit 0
fi
case "$1" in
  list-sessions)
    printf 'kwt-wt-work-a1b2c3d4:1:1:0:sh:/tmp\n'
    printf 'kwt-wt-work-e5f6a7b8:1:1:0:sh:/tmp\n'
    ;;
  has-session|kill-session) exit 0 ;;
  *) exit 1 ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700))
	args := []string{"tmux", "kill", pattern}
	if flag != "" {
		args = append(args, flag)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKwtHelperProcess$")
	cmd.Env = append(os.Environ(), "PATH="+dir, "KWT_HOME="+filepath.Join(dir, "home"),
		"KWT_FAKE_LOG="+logPath, "TERM=", "KWT_TEST_ARGS="+strings.Join(args, "\n"))
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	require.NoError(t, ctx.Err(), "helper timed out: %s", output)
	calls, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return string(output), string(calls), err
}
