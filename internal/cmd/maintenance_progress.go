package cmd

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type maintenanceProgress interface {
	Phase(name string, total int)
	Set(completed int)
	Pause()
	Resume()
	Close()
}

type noopMaintenanceProgress struct{}

func (noopMaintenanceProgress) Phase(string, int) {}
func (noopMaintenanceProgress) Set(int)           {}
func (noopMaintenanceProgress) Pause()            {}
func (noopMaintenanceProgress) Resume()           {}
func (noopMaintenanceProgress) Close()            {}

type maintenanceProgressOptions struct {
	Writer   io.Writer
	Terminal bool
	Now      func() time.Time
	Ticks    <-chan time.Time
}

type maintenanceProgressState struct {
	writer        io.Writer
	terminal      bool
	now           func() time.Time
	phase         string
	total         int
	completed     int
	phaseStarted  time.Time
	lastMilestone int
	frame         int
	paused        bool
	closed        bool
	stop          chan struct{}
	done          chan struct{}
	stopTicker    func()
	mu            sync.Mutex
}

var maintenanceProgressIsTerminal = func(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func newMaintenanceProgress(cmd *cobra.Command, enabled bool) maintenanceProgress {
	if !enabled {
		return noopMaintenanceProgress{}
	}
	writer := cmd.ErrOrStderr()
	return newMaintenanceProgressWithOptions(maintenanceProgressOptions{
		Writer:   writer,
		Terminal: maintenanceProgressIsTerminal(writer),
		Now:      time.Now,
	})
}

func newMaintenanceProgressWithOptions(options maintenanceProgressOptions) maintenanceProgress {
	if options.Writer == nil {
		options.Writer = io.Discard
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	progress := &maintenanceProgressState{
		writer:   options.Writer,
		terminal: options.Terminal,
		now:      options.Now,
	}
	if !options.Terminal {
		return progress
	}

	ticks := options.Ticks
	if ticks == nil {
		ticker := time.NewTicker(100 * time.Millisecond)
		ticks = ticker.C
		progress.stopTicker = ticker.Stop
	}
	progress.stop = make(chan struct{})
	progress.done = make(chan struct{})
	go progress.animate(ticks)
	return progress
}

func (p *maintenanceProgressState) animate(ticks <-chan time.Time) {
	defer close(p.done)
	for {
		select {
		case <-p.stop:
			return
		case <-ticks:
			p.mu.Lock()
			if !p.closed && !p.paused && p.phase != "" {
				p.frame++
				p.renderLocked()
			}
			p.mu.Unlock()
		}
	}
}

func (p *maintenanceProgressState) Phase(name string, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.phase = name
	p.total = max(total, 0)
	p.completed = 0
	p.phaseStarted = p.now()
	p.lastMilestone = 0
	if p.terminal {
		p.renderLocked()
		return
	}
	_, _ = fmt.Fprintf(p.writer, "kwt: %s\n", name)
}

func (p *maintenanceProgressState) Set(completed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.phase == "" {
		return
	}
	completed = max(completed, 0)
	if p.total > 0 {
		completed = min(completed, p.total)
	}
	p.completed = completed
	if p.terminal {
		if !p.paused {
			p.renderLocked()
		}
		return
	}
	if p.total == 0 {
		return
	}
	milestone := min(completed*4/p.total, 4)
	if milestone <= p.lastMilestone {
		return
	}
	p.lastMilestone = milestone
	_, _ = fmt.Fprintf(p.writer, "kwt: %s %d/%d\n", p.phase, completed, p.total)
}

func (p *maintenanceProgressState) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.paused {
		return
	}
	p.paused = true
	if p.terminal {
		p.clearLocked()
	}
}

func (p *maintenanceProgressState) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.paused {
		return
	}
	p.paused = false
	if p.terminal && p.phase != "" {
		p.renderLocked()
	}
}

func (p *maintenanceProgressState) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.terminal {
		p.clearLocked()
		close(p.stop)
	}
	p.mu.Unlock()

	if p.stopTicker != nil {
		p.stopTicker()
	}
	if p.done != nil {
		<-p.done
	}
}

func (p *maintenanceProgressState) renderLocked() {
	frames := "|/-\\"
	_, _ = fmt.Fprintf(p.writer, "\r\x1b[2K%c %s", frames[p.frame%len(frames)], p.phase)
	if p.total <= 0 {
		return
	}
	_, _ = fmt.Fprintf(p.writer, " %d/%d", p.completed, p.total)
	if p.completed <= 0 || p.completed >= p.total {
		return
	}
	elapsed := p.now().Sub(p.phaseStarted)
	remaining := time.Duration(float64(elapsed) / float64(p.completed) * float64(p.total-p.completed)).Round(time.Second)
	_, _ = fmt.Fprintf(p.writer, " · ETA %s", remaining)
}

func (p *maintenanceProgressState) clearLocked() {
	_, _ = io.WriteString(p.writer, "\r\x1b[2K")
}
