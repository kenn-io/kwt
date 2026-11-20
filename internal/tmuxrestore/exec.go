package tmuxrestore

import (
	"cmp"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ExecTarget is the program a restored pane runs in place of its handoff.
type ExecTarget struct {
	// Path is the program file Argv[0] names.
	Path string
	Argv []string
	Dir  string
}

// safePath matches the kwt paths restore will type into a shell inside single
// quotes. Every listed character is literal there in sh, bash, zsh, and fish;
// letters, marks, and digits from any script are allowed, so a home directory
// in the user's language works.
var safePath = regexp.MustCompile(`^/[\p{L}\p{M}\p{N} /._+,:@=~-]+$`)

func safeKwtPath(path string) bool { return safePath.MatchString(path) }

// Claim gives a waiting launch to the pane it was typed into and marks it
// started, so it runs once. tmuxPane and serverPID come from the pane's own
// environment: a line copied into any other pane, or run again, is refused.
// A program that cannot be found is marked failed instead, so the restore
// never reports it started.
func Claim(ctx context.Context, store Store, sock Socket, entryID string, launch int, tmuxPane, serverPID string) (ExecTarget, error) {
	var target ExecTarget
	var lookupErr error
	err := store.WithLock(ctx, sock, func(f *Files) error {
		entry := f.Entry(entryID)
		if entry == nil || entry.State != StateLaunching {
			return errors.New("no restore is starting programs with this ID")
		}
		if launch < 0 || launch >= len(entry.Launches) {
			return errors.New("no such launch in this restore")
		}
		l := &entry.Launches[launch]
		if l.PaneID != tmuxPane || entry.LiveServer.PID != serverPID {
			return errors.New("this is not the restored pane")
		}
		switch {
		case l.Status == LaunchStarted:
			return errors.New("this pane's program already started")
		case l.Status != LaunchWaiting || len(l.Argv) == 0:
			return errors.New("this pane has nothing to start")
		}
		path, err := programPath(l.Argv[0], l.Dir)
		if err != nil {
			l.Status, l.Note = LaunchFailed, notStarted(err)
			lookupErr = err
			return nil
		}
		l.Status = LaunchStarted
		target = ExecTarget{Path: path, Argv: l.Argv, Dir: l.Dir}
		return nil
	})
	return target, cmp.Or(err, lookupErr)
}

// programPath finds the file name runs, as the pane's shell would from dir.
func programPath(name, dir string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) && !filepath.IsAbs(name) {
		name = filepath.Join(dir, name)
	}
	return exec.LookPath(name)
}

func notStarted(err error) string { return "program did not start: " + err.Error() }

// Launch claims a launch and runs its program in place of this process. A
// program that cannot start is recorded as failed, so the restore does not
// report a conversation that never resumed. It returns only on failure.
func Launch(ctx context.Context, store Store, sock Socket, entryID string, launch int, tmuxPane, serverPID string) error {
	target, err := Claim(ctx, store, sock, entryID, launch, tmuxPane, serverPID)
	if err != nil {
		return err
	}
	execErr := Exec(target)
	_ = store.WithLock(ctx, sock, func(f *Files) error {
		entry := f.Entry(entryID)
		if entry == nil || launch >= len(entry.Launches) {
			return nil
		}
		if l := &entry.Launches[launch]; l.PaneID == tmuxPane && l.Status == LaunchStarted {
			l.Status, l.Note = LaunchFailed, notStarted(execErr)
		}
		return nil
	})
	return execErr
}
