package lifecycle

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

type fakeWorktreeProcess struct {
	processID        int32
	workingDirectory string
	workingDirErr    error
}

func (p fakeWorktreeProcess) pid() int32 { return p.processID }

func (p fakeWorktreeProcess) CwdWithContext(context.Context) (string, error) {
	return p.workingDirectory, p.workingDirErr
}

func TestProcessGuardRejectsProcessesInsideWorktree(t *testing.T) {
	err := rejectProcessCandidatesUsingWorktree(
		context.Background(),
		"/worktrees/topic",
		[]worktreeProcess{
			fakeWorktreeProcess{processID: 42, workingDirectory: "/worktrees/topic"},
			fakeWorktreeProcess{processID: 7, workingDirectory: "/worktrees/topic/nested"},
			fakeWorktreeProcess{processID: 9, workingDirectory: "/elsewhere"},
		},
	)

	var conditionErr *worktreeProcessConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Equal(t, []int32{7, 42}, conditionErr.pids)
	assert.NotContains(t, err.Error(), "9")
}

func TestProcessGuardIgnoresProcessesWithoutInspectableWorkingDirectory(t *testing.T) {
	err := rejectProcessCandidatesUsingWorktree(
		context.Background(),
		"/worktrees/topic",
		[]worktreeProcess{
			fakeWorktreeProcess{
				processID:     42,
				workingDirErr: errors.New("permission denied"),
			},
			fakeWorktreeProcess{processID: 43},
			fakeWorktreeProcess{processID: 44, workingDirectory: "/elsewhere"},
		},
	)

	require.NoError(t, err)
}

func TestProcessGuardDetectsLiveProcessWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper process requires a POSIX sleep command")
	}
	worktreePath := t.TempDir()
	command := exec.Command("sleep", "30")
	command.Dir = worktreePath
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	err := rejectProcessesUsingWorktree(context.Background(), worktreePath)

	var conditionErr *worktreeProcessConditionError
	require.ErrorAs(t, err, &conditionErr)
	assert.Contains(t, conditionErr.pids, int32(command.Process.Pid))

	require.NoError(t, command.Process.Kill())
	_ = command.Wait()
	assert.NoError(
		t,
		rejectProcessesUsingWorktree(context.Background(), worktreePath),
	)
}

func TestProcessGuardIgnoresLiveProcessesOutsideWorktree(t *testing.T) {
	worktreePath := filepath.Join(t.TempDir(), "never-created")

	assert.NoError(t, rejectProcessesUsingWorktree(context.Background(), worktreePath))
}

func TestClassifyRemovalErrorReportsLiveProcessCondition(t *testing.T) {
	err := classifyRemovalError(
		&worktreeProcessConditionError{pids: []int32{42}},
		RemovalResult{Path: "/worktrees/topic"},
	)

	assert.True(t, service.IsCode(err, service.Conflict))
	typed := service.AsError(err)
	assert.Equal(t, "process_working_directory_live", typed.Details["reason"])
}
