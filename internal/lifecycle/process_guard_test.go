package lifecycle

import (
	"context"
	"errors"
	"os/user"
	"testing"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

type fakeWorktreeProcess struct {
	processID        int32
	workingDirectory string
	workingDirErr    error
	statuses         []string
	username         string
}

func (p fakeWorktreeProcess) pid() int32 { return p.processID }

func (p fakeWorktreeProcess) CwdWithContext(context.Context) (string, error) {
	return p.workingDirectory, p.workingDirErr
}

func (p fakeWorktreeProcess) StatusWithContext(context.Context) ([]string, error) {
	return p.statuses, nil
}

func (p fakeWorktreeProcess) UsernameWithContext(context.Context) (string, error) {
	return p.username, nil
}

func TestProcessGuardRejectsUninspectableCurrentUserProcess(t *testing.T) {
	currentUser, err := user.Current()
	require.NoError(t, err)

	err = rejectProcessCandidatesUsingWorktree(
		context.Background(),
		"/worktrees/topic",
		[]worktreeProcess{fakeWorktreeProcess{
			processID:     42,
			workingDirErr: errors.New("permission denied"),
			statuses:      []string{"running"},
			username:      currentUser.Username,
		}},
	)

	var inspectionErr *worktreeProcessInspectionError
	require.ErrorAs(t, err, &inspectionErr)
	require.Contains(t, err.Error(), "PID 42")
}

func TestProcessGuardRejectsEmptyWorkingDirectoryForCurrentUserProcess(t *testing.T) {
	currentUser, err := user.Current()
	require.NoError(t, err)

	err = rejectProcessCandidatesUsingWorktree(
		context.Background(),
		"/worktrees/topic",
		[]worktreeProcess{fakeWorktreeProcess{
			processID: 42,
			statuses:  []string{"running"},
			username:  currentUser.Username,
		}},
	)

	var inspectionErr *worktreeProcessInspectionError
	require.ErrorAs(t, err, &inspectionErr)
	require.Contains(t, err.Error(), "PID 42")
}

func TestProcessGuardIgnoresZombieWithNoWorkingDirectory(t *testing.T) {
	currentUser, err := user.Current()
	require.NoError(t, err)

	err = rejectProcessCandidatesUsingWorktree(
		context.Background(),
		"/worktrees/topic",
		[]worktreeProcess{fakeWorktreeProcess{
			processID:     42,
			workingDirErr: errors.New("working directory unavailable"),
			statuses:      []string{process.Zombie},
			username:      currentUser.Username,
		}},
	)

	require.NoError(t, err)
}

func TestClassifyRemovalErrorReportsIndeterminateProcessInspection(t *testing.T) {
	err := classifyRemovalError(
		&worktreeProcessInspectionError{pids: []int32{42}},
		RemovalResult{Path: "/worktrees/topic"},
	)

	assert.True(t, service.IsCode(err, service.Conflict))
	typed := service.AsError(err)
	assert.Equal(t, "process_working_directory_indeterminate", typed.Details["reason"])
}
