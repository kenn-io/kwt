package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMaintenanceProgressTerminalReplacesOneLineAndUsesPerPhaseETA(t *testing.T) {
	var output bytes.Buffer
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time, 4)
	progress := newMaintenanceProgressWithOptions(maintenanceProgressOptions{
		Writer:   &output,
		Terminal: true,
		Now:      func() time.Time { return now },
		Ticks:    ticks,
	})

	progress.Phase("verify pull requests", 4)
	now = now.Add(2 * time.Second)
	progress.Set(1)
	progress.Phase("remove worktrees", 2)
	now = now.Add(time.Second)
	progress.Set(1)
	progress.Close()

	text := output.String()
	assert.Contains(t, text, "verify pull requests 1/4 · ETA 6s")
	assert.Contains(t, text, "remove worktrees 1/2 · ETA 1s")
	assert.NotContains(t, text, "ETA 3s", "ETA must reset at the phase boundary")
	assert.True(t, strings.HasSuffix(text, "\r\x1b[2K"))
}

func TestMaintenanceProgressNonTerminalEmitsOnlyQuartileMilestones(t *testing.T) {
	var output bytes.Buffer
	progress := newMaintenanceProgressWithOptions(maintenanceProgressOptions{
		Writer: &output,
		Now:    time.Now,
	})
	progress.Phase("inspect worktrees", 100)
	for completed := 1; completed <= 100; completed++ {
		progress.Set(completed)
	}
	progress.Close()

	assert.Equal(t, strings.Join([]string{
		"kwt: inspect worktrees",
		"kwt: inspect worktrees 25/100",
		"kwt: inspect worktrees 50/100",
		"kwt: inspect worktrees 75/100",
		"kwt: inspect worktrees 100/100",
		"",
	}, "\n"), output.String())
}

func TestMaintenanceProgressPauseClearsBeforePromptAndResumeRedraws(t *testing.T) {
	var output bytes.Buffer
	progress := newMaintenanceProgressWithOptions(maintenanceProgressOptions{
		Writer:   &output,
		Terminal: true,
		Now:      time.Now,
		Ticks:    make(chan time.Time),
	})
	progress.Phase("remove worktrees", 2)
	progress.Set(1)
	progress.Pause()
	_, _ = io.WriteString(&output, "Remove /worktree and all local files? [y/N] ")
	progress.Resume()
	progress.Close()

	assert.Contains(t, output.String(), "\r\x1b[2KRemove /worktree")
}
