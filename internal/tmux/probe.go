package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type ProtectedSessionState uint8

const (
	ProtectedSessionIndeterminate ProtectedSessionState = iota
	ProtectedSessionAbsent
	ProtectedSessionLive
)

type exitCoder interface{ ExitCode() int }

func ProbeProtectedSession(
	ctx context.Context,
	socketName string,
	expectedSession string,
) (ProtectedSessionState, error) {
	command := NewTmuxCommandForSocketWithStripNames("", socketName, nil)
	output, err := command.RunCommandOutputContext(
		ctx,
		"list-sessions",
		"-F",
		"#{session_name}\t#{session_attached}",
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
	}
	return classifyProtectedSessionProbe(expectedSession, output, err)
}

func classifyProtectedSessionProbe(
	expectedSession string,
	output string,
	err error,
) (ProtectedSessionState, error) {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ProtectedSessionIndeterminate, err
		}
		var exited exitCoder
		if errors.As(err, &exited) && exited.ExitCode() == 1 {
			return ProtectedSessionAbsent, nil
		}
		return ProtectedSessionIndeterminate, err
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 1 {
		return ProtectedSessionIndeterminate, fmt.Errorf("protected tmux server contains unexpected sessions")
	}
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 2 || fields[0] != expectedSession ||
		(fields[1] != "0" && fields[1] != "1") {
		return ProtectedSessionIndeterminate, fmt.Errorf("protected tmux server state is unexpected")
	}
	return ProtectedSessionLive, nil
}
