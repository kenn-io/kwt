package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.kenn.io/kwt/pkg/models"
)

type workspaceSessionEnsurer interface {
	Ensure(context.Context, string, string, models.Layout) error
	EnsureWithGeneration(context.Context, string, string, string, models.Layout) error
	RepairExisting(context.Context, string, string, models.Layout) error
	RepairExistingWithGeneration(context.Context, string, string, string, models.Layout) error
}

type workspaceAttachCommand interface {
	AttachSession(string) error
	SwitchClient(string) error
	AttachSessionNested(context.Context, string) error
	ServerPIDContext(context.Context) (string, error)
}

type workspaceResidentAttachCommand interface {
	workspaceAttachCommand
	AttachSessionNestedCommand(context.Context, string) *exec.Cmd
}

// WorkspaceSessions resolves, establishes, and presents KWT workspace
// sessions. Default-server adoption remains private to this coordinator.
type WorkspaceSessions struct {
	resolver             *endpointResolver
	servers              serverCommands
	resolveSession       func(context.Context, WorkspaceEndpointRequest) (resolvedWorkspaceSession, error)
	workspaceForEndpoint func(SessionEndpoint) (workspaceSessionEnsurer, error)
	attachForEndpoint    func(SessionEndpoint) (workspaceAttachCommand, error)
	tmuxEnvironment      func() string
}

func NewWorkspaceSessions(options WorkspaceSessionsOptions) *WorkspaceSessions {
	servers := newServerCommands(options)
	resolver := newEndpointResolver(
		servers.kwtServer(),
		servers.defaultServer(),
		options.ReportDiagnostic,
	)
	sessions := &WorkspaceSessions{
		resolver:       resolver,
		servers:        servers,
		resolveSession: resolver.resolve,
		tmuxEnvironment: func() string {
			return os.Getenv("TMUX")
		},
	}
	sessions.workspaceForEndpoint = func(
		endpoint SessionEndpoint,
	) (workspaceSessionEnsurer, error) {
		command, err := servers.commandForEndpoint(endpoint)
		if err != nil {
			return nil, err
		}
		return NewWorkspaceRunner(command, options.StripNames), nil
	}
	sessions.attachForEndpoint = func(
		endpoint SessionEndpoint,
	) (workspaceAttachCommand, error) {
		return servers.commandForEndpoint(endpoint)
	}
	return sessions
}

func (s *WorkspaceSessions) Resolve(
	ctx context.Context,
	request WorkspaceEndpointRequest,
) (WorkspaceSession, error) {
	resolved, err := s.resolver.resolve(ctx, request)
	if errors.Is(err, exec.ErrNotFound) {
		return newResolvedWorkspaceSession(
			request.SessionName,
			false,
			false,
		).WorkspaceSession, nil
	}
	return resolved.WorkspaceSession, err
}

func (s *WorkspaceSessions) ResolveAll(
	ctx context.Context,
	requests []WorkspaceEndpointRequest,
) ([]WorkspaceSession, error) {
	resolved, err := s.resolver.resolveAll(ctx, requests)
	if errors.Is(err, exec.ErrNotFound) {
		sessions := make([]WorkspaceSession, len(requests))
		for index := range requests {
			sessions[index] = newResolvedWorkspaceSession(
				requests[index].SessionName,
				false,
				false,
			).WorkspaceSession
		}
		return sessions, nil
	}
	if err != nil {
		return nil, err
	}
	sessions := make([]WorkspaceSession, len(resolved))
	for index := range resolved {
		sessions[index] = resolved[index].WorkspaceSession
	}
	return sessions, nil
}

// ResolveAllBestEffort resolves inventory endpoints without allowing one
// stale session to suppress unrelated workspace records. Establishment and
// attachment must continue to use Resolve or Establish, which fail closed.
func (s *WorkspaceSessions) ResolveAllBestEffort(
	ctx context.Context,
	requests []WorkspaceEndpointRequest,
) ([]WorkspaceSessionResolution, error) {
	resolved, err := s.resolver.resolveAllBestEffort(ctx, requests)
	if errors.Is(err, exec.ErrNotFound) {
		results := make([]WorkspaceSessionResolution, len(requests))
		for index := range requests {
			results[index].Session = newResolvedWorkspaceSession(
				requests[index].SessionName,
				false,
				false,
			).WorkspaceSession
		}
		return results, nil
	}
	return resolved, err
}

// LiveEndpoints returns every verified live endpoint for one workspace. It is
// intended for operations that must clean up duplicate sessions across shared
// servers rather than select one endpoint for attachment.
func (s *WorkspaceSessions) LiveEndpoints(
	ctx context.Context,
	request WorkspaceEndpointRequest,
) ([]SessionEndpoint, error) {
	endpoints, err := s.resolver.liveEndpoints(ctx, request)
	if errors.Is(err, exec.ErrNotFound) {
		return []SessionEndpoint{}, nil
	}
	return endpoints, err
}

func (s *WorkspaceSessions) Establish(
	ctx context.Context,
	session, workspacePath string,
	layout models.Layout,
) (SessionEndpoint, error) {
	return s.establish(ctx, session, workspacePath, "", layout)
}

func (s *WorkspaceSessions) EstablishWithGeneration(
	ctx context.Context,
	session, workspacePath, generation string,
	layout models.Layout,
) (SessionEndpoint, error) {
	return s.establish(ctx, session, workspacePath, generation, layout)
}

func (s *WorkspaceSessions) establish(
	ctx context.Context,
	session, workspacePath, generation string,
	layout models.Layout,
) (SessionEndpoint, error) {
	request := WorkspaceEndpointRequest{
		SessionName:         session,
		WorkspacePath:       workspacePath,
		WorkspaceGeneration: generation,
	}
	for attempt := 0; attempt < 2; attempt++ {
		resolved, err := s.resolveSession(ctx, request)
		if err != nil {
			return SessionEndpoint{}, err
		}
		effectiveSession := resolved.Endpoint.SessionName
		workspace, err := s.workspaceForEndpoint(resolved.Endpoint)
		if err != nil {
			return SessionEndpoint{}, err
		}
		repairOnly := resolved.Live && effectiveSession != request.SessionName
		if resolved.adopted {
			// compat(kag1): default-server adoption
			repairOnly = true
		}
		if repairOnly {
			if generation == "" {
				err = workspace.RepairExisting(
					ctx, effectiveSession, workspacePath, layout,
				)
			} else {
				err = workspace.RepairExistingWithGeneration(
					ctx, effectiveSession, workspacePath, generation, layout,
				)
			}
			if errors.Is(err, errWorkspaceSessionAbsent) && attempt == 0 {
				continue
			}
		} else if generation == "" {
			err = workspace.Ensure(
				ctx, effectiveSession, workspacePath, layout,
			)
		} else {
			err = workspace.EnsureWithGeneration(
				ctx, effectiveSession, workspacePath, generation, layout,
			)
		}
		if err != nil {
			return SessionEndpoint{}, err
		}
		return resolved.Endpoint, nil
	}
	return SessionEndpoint{}, errWorkspaceSessionAbsent
}

func (s *WorkspaceSessions) Attach(
	ctx context.Context,
	endpoint SessionEndpoint,
) error {
	command, err := s.attachForEndpoint(endpoint)
	if err != nil {
		return err
	}
	return attachWorkspaceEndpoint(
		ctx,
		endpoint,
		command,
		s.tmuxEnvironment(),
	)
}

// PrepareResidentAttach switches a client that is already on the resolved
// server. For a different server it returns the nested attach process without
// starting it, so the resident terminal owner can suspend itself first.
func (s *WorkspaceSessions) PrepareResidentAttach(
	ctx context.Context,
	endpoint SessionEndpoint,
) (*exec.Cmd, error) {
	rawCommand, err := s.attachForEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	command, ok := rawCommand.(workspaceResidentAttachCommand)
	if !ok {
		return nil, errors.New("tmux attach command cannot prepare a resident attach")
	}
	tmuxValue := s.tmuxEnvironment()
	if tmuxValue == "" {
		return nil, errors.New("resident tmux attach requires a current tmux client")
	}
	clientOnServer, err := clientOnResolvedServer(ctx, command, tmuxValue)
	if err != nil {
		return nil, err
	}
	if clientOnServer {
		return nil, command.SwitchClient(endpoint.SessionName)
	}
	process := command.AttachSessionNestedCommand(ctx, endpoint.SessionName)
	if process.Err != nil {
		return nil, process.Err
	}
	return process, nil
}

// Kill terminates the exact direct-attachment endpoint supplied by inventory.
func (s *WorkspaceSessions) Kill(endpoint SessionEndpoint) error {
	command, err := s.servers.commandForEndpoint(endpoint)
	if err != nil {
		return err
	}
	return command.KillSessionIfPresentContext(
		context.Background(),
		endpoint.SessionName,
	)
}

// KillMatching terminates only the session generation observed at the
// supplied endpoint. A same-named replacement is left untouched.
func (s *WorkspaceSessions) KillMatching(
	ctx context.Context,
	endpoint SessionEndpoint,
	request WorkspaceEndpointRequest,
) error {
	command, err := s.servers.commandForEndpoint(endpoint)
	if err != nil {
		return err
	}
	request.SessionName = endpoint.SessionName
	return killWorkspaceSessionIfMatching(ctx, command, request)
}

func attachWorkspaceEndpoint(
	ctx context.Context,
	endpoint SessionEndpoint,
	command workspaceAttachCommand,
	tmuxValue string,
) error {
	if tmuxValue == "" {
		return command.AttachSession(endpoint.SessionName)
	}
	clientOnServer, err := clientOnResolvedServer(ctx, command, tmuxValue)
	if err != nil {
		return err
	}
	if clientOnServer {
		return command.SwitchClient(endpoint.SessionName)
	}
	return command.AttachSessionNested(ctx, endpoint.SessionName)
}

func clientOnResolvedServer(
	ctx context.Context,
	command workspaceAttachCommand,
	tmuxValue string,
) (bool, error) {
	clientPID, err := parseTMUXServerPID(tmuxValue)
	if err != nil {
		return false, err
	}
	targetRaw, err := command.ServerPIDContext(ctx)
	if err != nil {
		return false, fmt.Errorf("read resolved tmux server PID: %w", err)
	}
	targetPID, err := parseCanonicalPID(strings.TrimSpace(targetRaw))
	if err != nil {
		return false, fmt.Errorf(
			"resolved tmux server did not return a numeric PID; tmux 2.1 or newer is required: %w",
			err,
		)
	}
	return clientPID == targetPID, nil
}
