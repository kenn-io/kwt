package tmuxrestore

import (
	"context"
	"errors"
	"regexp"
)

// ExecTarget is the program a restored pane runs in place of its handoff.
type ExecTarget struct {
	Argv []string
	Dir  string
}

// safePath matches the kwt paths restore will type into a shell inside single
// quotes. Every listed character is literal there in sh, bash, zsh, and fish.
var safePath = regexp.MustCompile(`^/[A-Za-z0-9 /._+,:@=~-]+$`)

func safeKwtPath(path string) bool { return safePath.MatchString(path) }

// Claim gives a waiting launch to the pane it was typed into and marks it
// started, so it runs once. tmuxPane and serverPID come from the pane's own
// environment: a line copied into any other pane, or run again, is refused.
func Claim(ctx context.Context, store Store, sock Socket, entryID string, launch int, tmuxPane, serverPID string) (ExecTarget, error) {
	var target ExecTarget
	err := store.WithLock(ctx, sock, func(f *Files) error {
		entry := f.Entry(entryID)
		if entry == nil || entry.State != StateLaunching {
			return errors.New("no restore is starting programs with this ID")
		}
		if launch < 0 || launch >= len(entry.Launches) {
			return errors.New("no such launch in this restore")
		}
		l := &entry.Launches[launch]
		if l.PaneID != tmuxPane || entry.LiveServerPID != serverPID {
			return errors.New("this is not the restored pane")
		}
		switch {
		case l.Status == LaunchStarted:
			return errors.New("this pane's program already started")
		case l.Status != LaunchWaiting || len(l.Argv) == 0:
			return errors.New("this pane has nothing to start")
		}
		l.Status = LaunchStarted
		target = ExecTarget{Argv: l.Argv, Dir: l.Dir}
		return nil
	})
	return target, err
}
