package tmuxrestore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwttmux "go.kenn.io/kwt/internal/tmux"
)

const (
	worktreeSession    = "kwt-wt-restore-main-01234567"
	worktreeGeneration = "0123456789abcdef0123456789abcdef"
)

// worktreeFixture is a restoreFixture on kwt's own server in a private
// TMUX_TMPDIR, so kwt's worktree create path and restore share one server.
type worktreeFixture struct {
	*restoreFixture
	tempDir  string
	worktree string
}

func newWorktreeFixture(t *testing.T) *worktreeFixture {
	t.Helper()
	f := &worktreeFixture{restoreFixture: newRestoreFixture(t), worktree: t.TempDir()}
	tempDir, err := os.MkdirTemp("/tmp", "kwr")
	require.NoError(t, err)
	f.tempDir = tempDir
	socket, err := SocketIn(tempDir, kwttmux.KWTServerSocketName)
	require.NoError(t, err)
	f.tm = Tmux{Executable: f.tm.Executable, Socket: socket}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_, _ = f.tm.Run(ctx, "kill-server")
		_ = os.RemoveAll(tempDir)
	})
	return f
}

// open runs kwt's worktree open path, offering restore when set.
func (f *worktreeFixture) open(t *testing.T, generation string, restore kwttmux.SkeletonRestorer) error {
	t.Helper()
	sessions := kwttmux.NewWorkspaceSessions(kwttmux.WorkspaceSessionsOptions{
		Command:              f.tm.Executable,
		KWTServerTempDir:     f.tempDir,
		DefaultServerTempDir: f.tempDir,
		Restore:              restore,
	})
	_, err := sessions.EstablishWithGeneration(t.Context(), worktreeSession, f.worktree, generation, kwttmux.BlankLayout())
	return err
}

// loseTwoPanes opens the worktree session, splits it, records a conversation
// in the new pane, and loses the server. It returns the saved pane geometry.
func (f *worktreeFixture) loseTwoPanes(t *testing.T) string {
	t.Helper()
	require.NoError(t, f.open(t, worktreeGeneration, nil))
	sub := filepath.Join(f.worktree, "sub")
	require.NoError(t, os.Mkdir(sub, 0o700))
	mustRun(t, f.tm, "split-window", "-h", "-t", "="+worktreeSession+":", "-c", sub)
	setResume(t, f.tm, "="+worktreeSession+":.1", claudeRecord("c-1", "claude"))
	geometry := f.geometry(t)
	f.lose(t)
	return geometry
}

func (f *worktreeFixture) geometry(t *testing.T) string {
	t.Helper()
	return mustRun(t, f.tm, "list-panes", "-t", "="+worktreeSession+":", "-F",
		"#{pane_index} #{pane_left} #{pane_top} #{pane_width} #{pane_height} #{pane_active}")
}

func (f *worktreeFixture) restoreHook(captured *EntryReport) kwttmux.SkeletonRestorer {
	return func(ctx context.Context, req kwttmux.SkeletonRequest) (bool, error) {
		report, restored, err := RestoreWorkspace(ctx, f.deps(), req)
		if captured != nil {
			*captured = report
		}
		return restored, err
	}
}

func TestOpeningAWorktreeRestoresItsSavedSession(t *testing.T) {
	f := newWorktreeFixture(t)
	saved := f.loseTwoPanes(t)
	id := f.entryNamed(t, worktreeSession).ID

	var report EntryReport
	require.NoError(t, f.open(t, worktreeGeneration, f.restoreHook(&report)))

	assert.Equal(t, saved, f.geometry(t), "the saved layout, not the blank preset")
	assert.Equal(t, OutcomePartial, report.Outcome, "the fake kwt never claims its launch")
	panes := strings.Fields(mustRun(t, f.tm, "list-panes", "-t", "="+worktreeSession+":", "-F", "#{pane_id}"))
	require.Len(t, panes, 2)
	assert.Equal(t, []string{"session exec " + id + " 1"}, f.handoffs(t, panes[1]))
	assert.Equal(t, kwttmux.WorkspacePathIdentity(f.worktree),
		strings.TrimSpace(mustRun(t, f.tm, "show-options", "-v", "-t", "="+worktreeSession+":", "@kwt-workspace-id")),
		"kwt's create path still bootstraps the session")
	assert.Equal(t, StateLaunching, f.entryNamed(t, worktreeSession).State)
}

func TestOpeningAWorktreeFirstAfterTheServerIsLostRestoresIt(t *testing.T) {
	// Right after a reboot, no save has seen the server go yet.
	f := newWorktreeFixture(t)
	require.NoError(t, f.open(t, worktreeGeneration, nil))
	mustRun(t, f.tm, "split-window", "-h", "-t", "="+worktreeSession+":", "-c", f.worktree)
	saved := f.geometry(t)
	f.save(t)
	mustRun(t, f.tm, "kill-server")

	require.NoError(t, f.open(t, worktreeGeneration, f.restoreHook(nil)))

	assert.Equal(t, saved, f.geometry(t), "the saved layout, not the blank preset")
}

func TestOpeningAWorktreeWhoseSaveFailsKeepsItsSavedSession(t *testing.T) {
	// A new server is running, but kwt cannot read it, so it cannot make
	// the lost server's snapshot restorable.
	f := newWorktreeFixture(t)
	require.NoError(t, f.open(t, worktreeGeneration, nil))
	mustRun(t, f.tm, "split-window", "-h", "-t", "="+worktreeSession+":", "-c", f.worktree)
	f.save(t)
	mustRun(t, f.tm, "kill-server")
	startServer(t, f.tm, "-s", "other")
	broken := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = list-panes ] && exit 1; done\nexec '" + f.tm.Executable + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(broken, []byte(script), 0o700))
	hook := func(ctx context.Context, req kwttmux.SkeletonRequest) (bool, error) {
		deps := f.deps()
		deps.Executable = broken
		_, restored, err := RestoreWorkspace(ctx, deps, req)
		return restored, err
	}

	err := f.open(t, worktreeGeneration, hook)

	require.ErrorContains(t, err, "save tmux sessions")
	_, err = f.tm.Run(t.Context(), "has-session", "-t", "="+worktreeSession)
	assert.Error(t, err, "no preset session takes the saved session's name")
}

func TestOpeningADirectoryWorkspaceRestoresItsSavedSession(t *testing.T) {
	// A registered directory has kwt's workspace markers but no checkout
	// generation.
	f := newWorktreeFixture(t)
	require.NoError(t, f.open(t, "", nil))
	mustRun(t, f.tm, "split-window", "-h", "-t", "="+worktreeSession+":", "-c", f.worktree)
	saved := f.geometry(t)
	f.lose(t)

	require.NoError(t, f.open(t, "", f.restoreHook(nil)))

	assert.Equal(t, saved, f.geometry(t), "the saved layout, not the blank preset")
	assert.Equal(t, kwttmux.WorkspacePathIdentity(f.worktree),
		strings.TrimSpace(mustRun(t, f.tm, "show-options", "-v", "-t", "="+worktreeSession+":", "@kwt-workspace-id")))
}

func TestOpeningAChangedWorktreeLeavesItsSavedSession(t *testing.T) {
	f := newWorktreeFixture(t)
	f.loseTwoPanes(t)

	other := "fedcba9876543210fedcba9876543210"
	require.NoError(t, f.open(t, other, f.restoreHook(nil)))

	panes := strings.Fields(mustRun(t, f.tm, "list-panes", "-t", "="+worktreeSession+":", "-F", "#{pane_id}"))
	assert.Len(t, panes, 1, "the preset builds a checkout the snapshot does not describe")
	assert.Equal(t, StatePending, f.entryNamed(t, worktreeSession).State)
}

// workspaceOpener runs kwt's open path as the session command does, minus
// the git identity guard.
func (f *worktreeFixture) workspaceOpener(t *testing.T, generation string) func(context.Context, Entry, kwttmux.SkeletonRestorer) error {
	return func(_ context.Context, entry Entry, restore kwttmux.SkeletonRestorer) error {
		assert.Equal(t, f.worktree, entry.Session.Path)
		return f.open(t, generation, restore)
	}
}

func TestRestoreOpensWorktreeSessionsThroughKwt(t *testing.T) {
	f := newWorktreeFixture(t)
	saved := f.loseTwoPanes(t)
	id := f.entryNamed(t, worktreeSession).ID
	deps := f.deps()
	deps.Workspace = f.workspaceOpener(t, worktreeGeneration)

	reports := f.restore(t, deps, id)

	require.Len(t, reports, 1)
	assert.Equal(t, OutcomePartial, reports[0].Outcome, reports[0].Reason)
	require.NotNil(t, reports[0].Live)
	assert.Equal(t, saved, f.geometry(t))
}

func TestRestoreSkipsAWorktreeSessionWhoseCheckoutChanged(t *testing.T) {
	f := newWorktreeFixture(t)
	f.loseTwoPanes(t)
	id := f.entryNamed(t, worktreeSession).ID
	deps := f.deps()
	deps.Workspace = f.workspaceOpener(t, "fedcba9876543210fedcba9876543210")

	reports := f.restore(t, deps, id)

	require.Len(t, reports, 1)
	assert.Equal(t, OutcomeSkipped, reports[0].Outcome)
	assert.Equal(t, "worktree changed", reports[0].Reason)
	assert.Equal(t, StatePending, f.entryNamed(t, worktreeSession).State)
	_, err := f.tm.Run(t.Context(), "has-session", "-t", "="+worktreeSession)
	assert.Error(t, err, "no preset session is built in its place")
}

func TestRestoreWithoutAWorkspaceOpenerSkipsWorktreeSessions(t *testing.T) {
	f := newWorktreeFixture(t)
	f.loseTwoPanes(t)

	reports := f.restore(t, f.deps(), f.entryNamed(t, worktreeSession).ID)

	require.Len(t, reports, 1)
	assert.Equal(t, OutcomeSkipped, reports[0].Outcome)
	assert.Equal(t, "worktree session", reports[0].Reason)
}
