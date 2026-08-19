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

type workspaceCleanupIdentity struct {
	SessionID           string
	WorkspaceIdentity   string
	WorkspaceGeneration string
	Absent              bool
}

type workspaceCleanupCommand interface {
	killWorkspaceSessionWithOptionContext(
		context.Context,
		string,
		string,
	) (workspaceCleanupResult, error)
	workspaceSessionCleanupIdentityContext(
		context.Context,
		string,
	) (workspaceCleanupIdentity, error)
	killWorkspaceSessionWithObservedIdentityContext(
		context.Context,
		string,
		string,
		string,
		string,
	) (workspaceCleanupResult, error)
}

func killWorkspaceSessionIfMatching(
	ctx context.Context,
	command workspaceCleanupCommand,
	request WorkspaceEndpointRequest,
) error {
	expectedIdentity := request.WorkspaceGeneration
	markerOption := workspaceGenerationOption
	mismatchReason := "belongs to a different worktree generation"
	malformedReason := "has malformed worktree generation markers"
	identityLength := 32
	if expectedIdentity != "" {
		if !validLowerHex(expectedIdentity, identityLength) {
			return workspaceCleanupSafetyError(
				request.SessionName,
				malformedReason,
			)
		}
	} else {
		expectedIdentity = workspacePathIdentity(request.WorkspacePath)
		markerOption = workspaceIdentityOption
		mismatchReason = "belongs to a different workspace identity"
		malformedReason = "has malformed workspace identity markers"
		identityLength = 64
	}

	result, err := command.killWorkspaceSessionWithOptionContext(
		ctx,
		request.SessionName,
		workspaceCleanupOption(expectedIdentity),
	)
	if err != nil {
		return err
	}
	if result != workspaceCleanupMismatch {
		return nil
	}

	// compat(kag1): sessions created before atomic cleanup markers
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
		return workspaceCleanupSafetyError(
			request.SessionName,
			"has malformed cleanup identity",
		)
	}
	observedIdentity := identity.WorkspaceGeneration
	if markerOption == workspaceIdentityOption {
		observedIdentity = identity.WorkspaceIdentity
	}
	if !validLowerHex(observedIdentity, identityLength) {
		return workspaceCleanupSafetyError(request.SessionName, malformedReason)
	}
	if observedIdentity != expectedIdentity {
		return workspaceCleanupSafetyError(request.SessionName, mismatchReason)
	}

	result, err = command.killWorkspaceSessionWithObservedIdentityContext(
		ctx,
		request.SessionName,
		identity.SessionID,
		markerOption,
		expectedIdentity,
	)
	if err != nil {
		return err
	}
	if result == workspaceCleanupMismatch {
		return workspaceCleanupSafetyError(request.SessionName, mismatchReason)
	}
	return nil
}

func workspaceCleanupSafetyError(session, reason string) error {
	return &SessionSafetyError{Reason: fmt.Sprintf(
		"tmux session %q %s",
		session,
		reason,
	)}
}

// KillWorkspaceSessionIfMatchingContext terminates a cleanup target only when
// its KWT identity marker still matches the removed workspace.
func (t *TmuxCommand) KillWorkspaceSessionIfMatchingContext(
	ctx context.Context,
	request WorkspaceEndpointRequest,
) error {
	return killWorkspaceSessionIfMatching(ctx, t, request)
}

// KillProtectedWorkspaceSessionsIfMatchingContext terminates every verified
// protected session at the canonical socket and the explicit socket directory
// used by earlier KWT versions. Each endpoint is checked independently so a
// live canonical session cannot hide a matching prior-location session.
func KillProtectedWorkspaceSessionsIfMatchingContext(
	ctx context.Context,
	socketName string,
	protectedNames []string,
	legacyTempDir string,
	request WorkspaceEndpointRequest,
) error {
	type protectedCleanupTarget struct {
		location string
		command  *TmuxCommand
	}
	targets := []protectedCleanupTarget{{
		location: "canonical",
		command:  newProtectedSessionProbeCommand(socketName, protectedNames),
	}}
	if legacyTempDir != "" {
		// compat(kag1): protected sessions created with inherited TMUX_TMPDIR
		targets = append(targets, protectedCleanupTarget{
			location: "legacy",
			command: NewTmuxCommandForSocketInTempDirWithStripNames(
				"", socketName, legacyTempDir, protectedNames,
			),
		})
	}

	var cleanupErr error
	for _, target := range targets {
		state, err := probeProtectedSessionCommand(
			ctx,
			target.command,
			request.SessionName,
		)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf(
				"inspect %s protected tmux endpoint: %w",
				target.location,
				err,
			))
			continue
		}
		switch state {
		case ProtectedSessionAbsent:
			continue
		case ProtectedSessionLive:
			err = target.command.KillWorkspaceSessionIfMatchingContext(ctx, request)
		default:
			err = fmt.Errorf("protected tmux session state is indeterminate")
		}
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf(
				"clean up %s protected tmux endpoint: %w",
				target.location,
				err,
			))
		}
	}
	return cleanupErr
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
	return classifyWorkspaceCleanupResult(ctx, output, stderr, err)
}

// compat(kag1): sessions created before atomic cleanup markers
func (t *TmuxCommand) workspaceSessionCleanupIdentityContext(
	ctx context.Context,
	session string,
) (workspaceCleanupIdentity, error) {
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
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return workspaceCleanupIdentity{}, err
		}
		var exited exitCoder
		if errors.As(err, &exited) && exited.ExitCode() == 1 &&
			isExplicitlyAbsentTmuxDiagnostic(stderr) {
			return workspaceCleanupIdentity{Absent: true}, nil
		}
		return workspaceCleanupIdentity{}, fmt.Errorf(
			"inspect tmux cleanup identity: %w, stderr: %s",
			err,
			strings.TrimSpace(stderr),
		)
	}
	fields := strings.Split(strings.TrimSuffix(output, "\n"), "|")
	if len(fields) != 3 {
		return workspaceCleanupIdentity{}, fmt.Errorf(
			"inspect tmux cleanup identity: malformed response",
		)
	}
	return workspaceCleanupIdentity{
		SessionID:           strings.TrimSuffix(fields[0], "\r"),
		WorkspaceIdentity:   strings.TrimSuffix(fields[1], "\r"),
		WorkspaceGeneration: strings.TrimSuffix(fields[2], "\r"),
	}, nil
}

// compat(kag1): sessions created before atomic cleanup markers
func (t *TmuxCommand) killWorkspaceSessionWithObservedIdentityContext(
	ctx context.Context,
	session string,
	sessionID string,
	markerOption string,
	expectedIdentity string,
) (workspaceCleanupResult, error) {
	showMarkerArgs := append(
		[]string{t.command},
		t.socketArgs([]string{
			"show-options", "-v", "-t", sessionID, markerOption,
		})...,
	)
	quotedShowMarkerArgs := make([]string, len(showMarkerArgs))
	for index, argument := range showMarkerArgs {
		quotedShowMarkerArgs[index] = quotePOSIXShellArgument(argument)
	}
	condition := "test \"$(" + strings.Join(quotedShowMarkerArgs, " ") +
		" 2>/dev/null)\" = " + quotePOSIXShellArgument(expectedIdentity)
	killCommand := "kill-session -t " + quoteTmuxCommandArgument(sessionID)
	output, stderr, err := t.runCommandOutputContextWithStderr(
		ctx,
		"if-shell",
		"-t",
		session,
		condition,
		killCommand,
		"display-message -p "+workspaceCleanupMismatchOutput,
	)
	return classifyWorkspaceCleanupResult(ctx, output, stderr, err)
}

func classifyWorkspaceCleanupResult(
	ctx context.Context,
	output string,
	stderr string,
	err error,
) (workspaceCleanupResult, error) {
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

func quotePOSIXShellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
