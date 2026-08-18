package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type sessionCleanupIdentity struct {
	SessionID           string
	WorkspaceIdentity   string
	WorkspaceGeneration string
	Absent              bool
}

type workspaceCleanupCommand interface {
	workspaceSessionCleanupIdentityContext(
		context.Context,
		string,
	) (sessionCleanupIdentity, error)
	KillSessionIfPresentContext(context.Context, string) error
}

func killWorkspaceSessionIfMatching(
	ctx context.Context,
	command workspaceCleanupCommand,
	request WorkspaceEndpointRequest,
) error {
	identity, err := command.workspaceSessionCleanupIdentityContext(
		ctx,
		request.SessionName,
	)
	if err != nil {
		return err
	}
	if identity.Absent {
		return nil
	}
	if !validRemovalSessionID(identity.SessionID) {
		return &SessionSafetyError{Reason: fmt.Sprintf(
			"tmux session %q has malformed cleanup identity",
			request.SessionName,
		)}
	}
	if request.WorkspaceGeneration != "" {
		if !validLowerHex(identity.WorkspaceGeneration, 32) {
			return &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q has malformed worktree generation markers",
				request.SessionName,
			)}
		}
		if identity.WorkspaceGeneration != request.WorkspaceGeneration {
			return &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q belongs to a different worktree generation",
				request.SessionName,
			)}
		}
	} else {
		if !validLowerHex(identity.WorkspaceIdentity, 64) {
			return &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q has malformed workspace identity markers",
				request.SessionName,
			)}
		}
		if identity.WorkspaceIdentity != workspacePathIdentity(request.WorkspacePath) {
			return &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q belongs to a different workspace identity",
				request.SessionName,
			)}
		}
	}
	return command.KillSessionIfPresentContext(ctx, identity.SessionID)
}

// KillWorkspaceSessionIfMatchingContext terminates a cleanup target only when
// its KWT identity markers still match the removed workspace.
func (t *TmuxCommand) KillWorkspaceSessionIfMatchingContext(
	ctx context.Context,
	request WorkspaceEndpointRequest,
) error {
	return killWorkspaceSessionIfMatching(ctx, t, request)
}

func (t *TmuxCommand) workspaceSessionCleanupIdentityContext(
	ctx context.Context,
	session string,
) (sessionCleanupIdentity, error) {
	output, stderr, err := t.runCommandOutputContextWithStderr(
		ctx,
		"display-message",
		"-p",
		"-t",
		session,
		"#{session_id}|#{@kwt-workspace-id}|#{@kwt-workspace-generation}",
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
	}
	return classifySessionCleanupIdentity(output, stderr, err)
}

func classifySessionCleanupIdentity(
	output string,
	stderr string,
	err error,
) (sessionCleanupIdentity, error) {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return sessionCleanupIdentity{}, err
		}
		var exited exitCoder
		if errors.As(err, &exited) && exited.ExitCode() == 1 &&
			isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return sessionCleanupIdentity{Absent: true}, nil
		}
		return sessionCleanupIdentity{}, fmt.Errorf(
			"inspect tmux cleanup identity: %w, stderr: %s",
			err,
			strings.TrimSpace(stderr),
		)
	}
	fields := strings.Split(strings.TrimSuffix(output, "\n"), "|")
	if len(fields) != 3 {
		return sessionCleanupIdentity{}, fmt.Errorf(
			"inspect tmux cleanup identity: malformed response",
		)
	}
	return sessionCleanupIdentity{
		SessionID:           strings.TrimSuffix(fields[0], "\r"),
		WorkspaceIdentity:   strings.TrimSuffix(fields[1], "\r"),
		WorkspaceGeneration: strings.TrimSuffix(fields[2], "\r"),
	}, nil
}
