package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type workspaceCleanupResult uint8

const (
	workspaceCleanupTerminated workspaceCleanupResult = iota
	workspaceCleanupAbsent
	workspaceCleanupMismatch
)

const workspaceCleanupMismatchOutput = "kwt-cleanup-marker-mismatch"

type workspaceCleanupCommand interface {
	killWorkspaceSessionWithOptionContext(
		context.Context,
		string,
		string,
	) (workspaceCleanupResult, error)
}

func killWorkspaceSessionIfMatching(
	ctx context.Context,
	command workspaceCleanupCommand,
	request WorkspaceEndpointRequest,
) error {
	cleanupIdentity := request.WorkspaceGeneration
	mismatchReason := "belongs to a different worktree generation"
	if cleanupIdentity != "" {
		if !validLowerHex(cleanupIdentity, 32) {
			return &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q has malformed worktree generation markers",
				request.SessionName,
			)}
		}
	} else {
		cleanupIdentity = workspacePathIdentity(request.WorkspacePath)
		mismatchReason = "belongs to a different workspace identity"
	}

	result, err := command.killWorkspaceSessionWithOptionContext(
		ctx,
		request.SessionName,
		workspaceCleanupOption(cleanupIdentity),
	)
	if err != nil {
		return err
	}
	if result == workspaceCleanupMismatch {
		return &SessionSafetyError{Reason: fmt.Sprintf(
			"tmux session %q %s",
			request.SessionName,
			mismatchReason,
		)}
	}
	return nil
}

// KillWorkspaceSessionIfMatchingContext terminates a cleanup target only when
// its KWT identity marker still matches the removed workspace.
func (t *TmuxCommand) KillWorkspaceSessionIfMatchingContext(
	ctx context.Context,
	request WorkspaceEndpointRequest,
) error {
	return killWorkspaceSessionIfMatching(ctx, t, request)
}

func (t *TmuxCommand) killWorkspaceSessionWithOptionContext(
	ctx context.Context,
	session string,
	cleanupOption string,
) (workspaceCleanupResult, error) {
	// if-shell -F evaluates the marker and queues the selected branch inside
	// one server request. A server restart cannot redirect the kill to a new
	// server that reused the original session ID.
	condition := "#{?" + cleanupOption + ",1,0}"
	killCommand := "kill-session -t " + quoteTmuxCommandArgument("="+session)
	output, stderr, err := t.runCommandOutputContextWithStderr(
		ctx,
		"if-shell",
		"-F",
		"-t",
		session,
		condition,
		killCommand,
		"display-message -p "+workspaceCleanupMismatchOutput,
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return workspaceCleanupMismatch, err
		}
		var exited exitCoder
		if errors.As(err, &exited) && exited.ExitCode() == 1 &&
			isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return workspaceCleanupAbsent, nil
		}
		return workspaceCleanupMismatch, fmt.Errorf(
			"inspect and terminate tmux cleanup target: %w, stderr: %s",
			err,
			strings.TrimSpace(stderr),
		)
	}
	switch strings.TrimSpace(output) {
	case "":
		return workspaceCleanupTerminated, nil
	case workspaceCleanupMismatchOutput:
		return workspaceCleanupMismatch, nil
	default:
		return workspaceCleanupMismatch, fmt.Errorf(
			"inspect and terminate tmux cleanup target: malformed response",
		)
	}
}

func quoteTmuxCommandArgument(value string) string {
	value = strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
	).Replace(value)
	return `"` + value + `"`
}
