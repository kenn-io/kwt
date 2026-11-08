// Package workspace selects a runtime while retaining the shared workspace
// lifecycle and tmux's existing endpoint coordination.
package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"go.kenn.io/kwt/internal/herdrcore"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
)

type Sessions struct {
	*tmux.WorkspaceSessions
	core   *herdrcore.Client
	socket string
	names  []string
}

func New(cfg *models.Config, options tmux.WorkspaceSessionsOptions) *Sessions {
	s := &Sessions{WorkspaceSessions: tmux.NewWorkspaceSessions(options), names: append([]string(nil), options.StripNames...)}
	if cfg != nil && cfg.Workspace.Backend == "herdr-core" {
		s.socket = cfg.Workspace.CoreSocket
		s.core = herdrcore.NewWorkspaceClient(s.socket, s.names...)
	}
	return s
}
func ValidateLayout(cfg *models.Config, layout models.Layout) error {
	if cfg != nil && cfg.Workspace.Backend == "herdr-core" && len(layout.Panes) > 1 {
		return fmt.Errorf("Herdr Core supports one terminal; multi-pane layouts are unsupported")
	}
	return nil
}
func (s *Sessions) options(request tmux.WorkspaceEndpointRequest) (herdrcore.RunOptions, error) {
	executable, err := os.Executable()
	if err != nil {
		return herdrcore.RunOptions{}, err
	}
	mode := "workspace"
	if request.Protected {
		mode = "protected-workspace"
	}
	return herdrcore.RunOptions{Executable: executable, Directory: request.WorkspacePath, Identifier: request.SessionName, Context: mode, ProtectedNames: s.names}, nil
}
func (s *Sessions) endpoint(t *herdrcore.Terminal, request tmux.WorkspaceEndpointRequest) tmux.SessionEndpoint {
	name := request.SessionName
	if t != nil && t.Identifier() != "" {
		name = t.Identifier()
	}
	e := tmux.SessionEndpoint{SessionName: name, Runtime: "herdr-core", WorkspacePath: request.WorkspacePath, WorkspaceGeneration: request.WorkspaceGeneration, Protected: request.Protected, CoreSocket: s.socket}
	if t != nil {
		e.Address = t.Address
		e.CreatedMS = t.CreatedMS
	}
	return e
}
func (s *Sessions) Resolve(ctx context.Context, r tmux.WorkspaceEndpointRequest) (tmux.WorkspaceSession, error) {
	if s.core == nil {
		return s.WorkspaceSessions.Resolve(ctx, r)
	}
	opts, err := s.options(r)
	if err != nil {
		return tmux.WorkspaceSession{}, err
	}
	t, err := s.core.ObserveWorkspace(ctx, opts, r.WorkspaceGeneration)
	return tmux.WorkspaceSession{Endpoint: s.endpoint(t, r), Live: t != nil}, err
}
func (s *Sessions) ResolveAll(ctx context.Context, requests []tmux.WorkspaceEndpointRequest) ([]tmux.WorkspaceSession, error) {
	if s.core == nil {
		return s.WorkspaceSessions.ResolveAll(ctx, requests)
	}
	out := make([]tmux.WorkspaceSession, len(requests))
	for i, r := range requests {
		v, err := s.Resolve(ctx, r)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
func (s *Sessions) ResolveAllBestEffort(ctx context.Context, requests []tmux.WorkspaceEndpointRequest) ([]tmux.WorkspaceSessionResolution, error) {
	if s.core == nil {
		return s.WorkspaceSessions.ResolveAllBestEffort(ctx, requests)
	}
	out := make([]tmux.WorkspaceSessionResolution, len(requests))
	for i, r := range requests {
		out[i].Session, out[i].Err = s.Resolve(ctx, r)
	}
	return out, nil
}
func (s *Sessions) LiveEndpoints(ctx context.Context, r tmux.WorkspaceEndpointRequest) ([]tmux.SessionEndpoint, error) {
	if s.core == nil {
		return s.WorkspaceSessions.LiveEndpoints(ctx, r)
	}
	v, err := s.Resolve(ctx, r)
	if err != nil {
		return nil, err
	}
	if !v.Live {
		return []tmux.SessionEndpoint{}, nil
	}
	return []tmux.SessionEndpoint{v.Endpoint}, nil
}
func (s *Sessions) ResolveLive(ctx context.Context, r tmux.WorkspaceEndpointRequest) (tmux.SessionEndpoint, error) {
	if s.core == nil {
		return s.WorkspaceSessions.ResolveLive(ctx, r)
	}
	v, err := s.Resolve(ctx, r)
	if err != nil {
		return tmux.SessionEndpoint{}, err
	}
	if !v.Live {
		return tmux.SessionEndpoint{}, fmt.Errorf("no live Core workspace")
	}
	return v.Endpoint, nil
}
func (s *Sessions) Establish(ctx context.Context, name, path string, layout models.Layout) (tmux.SessionEndpoint, error) {
	return s.EstablishWithGeneration(ctx, name, path, "", layout)
}
func (s *Sessions) EstablishWithGeneration(ctx context.Context, name, path, generation string, layout models.Layout) (tmux.SessionEndpoint, error) {
	if s.core == nil {
		return s.WorkspaceSessions.EstablishWithGeneration(ctx, name, path, generation, layout)
	}
	return s.EstablishCore(ctx, tmux.WorkspaceEndpointRequest{SessionName: name, WorkspacePath: path, WorkspaceGeneration: generation}, layout)
}
func (s *Sessions) EstablishCore(ctx context.Context, r tmux.WorkspaceEndpointRequest, layout models.Layout) (tmux.SessionEndpoint, error) {
	if s.core == nil {
		return tmux.SessionEndpoint{}, fmt.Errorf("Core backend is not selected")
	}
	if len(layout.Panes) > 1 {
		return tmux.SessionEndpoint{}, fmt.Errorf("Herdr Core does not support multi-pane layouts")
	}
	opts, err := s.options(r)
	if err != nil {
		return tmux.SessionEndpoint{}, err
	}
	if len(layout.Panes) == 1 {
		opts.Command = layout.Panes[0]
	}
	t, err := s.core.EstablishWorkspace(ctx, opts, r.WorkspaceGeneration)
	if err != nil {
		return tmux.SessionEndpoint{}, err
	}
	return s.endpoint(t, r), nil
}
func (s *Sessions) EstablishExpectedWithGeneration(ctx context.Context, name, expected, path, generation string, layout models.Layout) (tmux.SessionEndpoint, error) {
	if s.core == nil {
		return s.WorkspaceSessions.EstablishExpectedWithGeneration(ctx, name, expected, path, generation, layout)
	}
	r := tmux.WorkspaceEndpointRequest{SessionName: name, WorkspacePath: path, WorkspaceGeneration: generation}
	live, err := s.Resolve(ctx, r)
	if err != nil {
		return tmux.SessionEndpoint{}, err
	}
	if live.Endpoint.SessionName != expected {
		return tmux.SessionEndpoint{}, tmux.ErrWorkspaceSessionChanged
	}
	return s.EstablishWithGeneration(ctx, name, path, generation, layout)
}
func (s *Sessions) current(ctx context.Context, e tmux.SessionEndpoint) (*herdrcore.Terminal, herdrcore.RunOptions, error) {
	if s.core == nil || e.Runtime != "herdr-core" || (e.CoreSocket != "" && e.CoreSocket != s.socket) {
		return nil, herdrcore.RunOptions{}, fmt.Errorf("workspace runtime route changed")
	}
	r := tmux.WorkspaceEndpointRequest{SessionName: e.SessionName, WorkspacePath: e.WorkspacePath, WorkspaceGeneration: e.WorkspaceGeneration, Protected: e.Protected}
	opts, err := s.options(r)
	if err != nil {
		return nil, opts, err
	}
	t, err := s.core.CurrentWorkspace(ctx, opts, e.WorkspaceGeneration, herdrcore.Terminal{Address: e.Address, CreatedMS: e.CreatedMS})
	return t, opts, err
}
func (s *Sessions) PrepareResidentAttach(ctx context.Context, e tmux.SessionEndpoint) (*exec.Cmd, error) {
	if e.Runtime != "herdr-core" {
		return s.WorkspaceSessions.PrepareResidentAttach(ctx, e)
	}
	t, _, err := s.current(ctx, e)
	if err != nil {
		return nil, err
	}
	if t.State != "live" {
		return nil, fmt.Errorf("Core workspace is finished")
	}
	return s.core.AttachCommand(ctx, t.Address), nil
}
func (s *Sessions) Attach(ctx context.Context, e tmux.SessionEndpoint) error {
	if e.Runtime != "herdr-core" {
		return s.WorkspaceSessions.Attach(ctx, e)
	}
	cmd, err := s.PrepareResidentAttach(ctx, e)
	if err != nil {
		return err
	}
	return cmd.Run()
}
func (s *Sessions) KillMatching(ctx context.Context, e tmux.SessionEndpoint, r tmux.WorkspaceEndpointRequest) error {
	if e.Runtime != "herdr-core" {
		return s.WorkspaceSessions.KillMatching(ctx, e, r)
	}
	if e.WorkspacePath != r.WorkspacePath || e.WorkspaceGeneration != r.WorkspaceGeneration {
		return fmt.Errorf("Core workspace registration changed")
	}
	_, opts, err := s.current(ctx, e)
	if err != nil {
		return err
	}
	return s.core.StopWorkspace(ctx, opts, r.WorkspaceGeneration, herdrcore.Terminal{Address: e.Address, CreatedMS: e.CreatedMS})
}
