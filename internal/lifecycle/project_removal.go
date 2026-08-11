package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type ProjectRemovalRequest struct {
	Path               string           `json:"path"`
	ExpectedRepository string           `json:"expected_repository"`
	Expansion          ExpansionContext `json:"expansion"`
}

type ProjectRemovalResult struct {
	Project models.Project `json:"project"`
}

type ProjectRemover interface {
	RemoveProject(context.Context, ProjectRemovalRequest) (ProjectRemovalResult, error)
}

type ProjectRemovalServiceOptions struct{ Home string }

type protectedSessionProbe func(
	context.Context,
	string,
	string,
) (tmux.ProtectedSessionState, error)

type projectRemovalService struct {
	home      string
	probe     protectedSessionProbe
	beforeCAS func()
}

type protectedEndpoint struct {
	SessionName string
	SocketName  string
	Generation  string
}

func NewProjectRemovalService(options ProjectRemovalServiceOptions) ProjectRemover {
	return newProjectRemovalService(options.Home, tmux.ProbeProtectedSession)
}

func newProjectRemovalService(home string, probe protectedSessionProbe) ProjectRemover {
	return &projectRemovalService{home: home, probe: probe}
}

func (s *projectRemovalService) RemoveProject(
	ctx context.Context,
	request ProjectRemovalRequest,
) (result ProjectRemovalResult, resultErr error) {
	identity, err := validateStableProjectIdentity(request.ExpectedRepository)
	if err != nil || request.Path == "" {
		return result, projectRemovalError(
			service.InvalidRequest,
			"project removal request is invalid",
			false,
			nil,
			err,
		)
	}
	if err := request.Expansion.validate(); err != nil {
		return result, projectRemovalError(
			service.InvalidRequest,
			"project removal expansion context is invalid",
			false,
			nil,
			err,
		)
	}
	release, err := acquireProjectFence(ctx, s.home, identity)
	if err != nil {
		return result, classifyProjectRemovalFailure(err)
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			resultErr = errors.Join(resultErr, projectRemovalInternal(releaseErr))
		}
	}()

	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(
		s.home,
		request.Expansion.expandPath,
	)
	if err != nil {
		return result, projectRemovalInternal(err)
	}
	registration, matches := findExactProjectRegistration(snapshot.Projects, request.Path)
	switch matches {
	case 0:
		return result, projectRemovalError(
			service.ProjectNotFound,
			"no project is registered at the exact path",
			false, nil, nil,
		)
	case 1:
	default:
		return result, projectRemovalError(
			service.UnregistrationFailed,
			"multiple project registrations use the exact path",
			false, nil, nil,
		)
	}
	actualIdentity, err := stableProjectIdentity(registration)
	if err != nil || actualIdentity != identity {
		return result, projectRemovalError(
			service.RegistrationChanged,
			"the project registration no longer matches the expected repository",
			true, nil, err,
		)
	}
	endpoints, err := s.loadProtectedEndpoints(ctx, registration, identity)
	if err != nil {
		return result, err
	}
	for _, endpoint := range endpoints {
		state, probeErr := s.probe(ctx, endpoint.SocketName, endpoint.SessionName)
		switch state {
		case tmux.ProtectedSessionAbsent:
			if probeErr == nil {
				continue
			}
		case tmux.ProtectedSessionLive:
			return result, projectRemovalError(
				service.ProtectedSessionLive,
				"a protected project session is live",
				false,
				map[string]any{
					"session_name": endpoint.SessionName,
					"socket_name":  endpoint.SocketName,
					"generation":   endpoint.Generation,
				},
				nil,
			)
		}
		return result, projectRemovalError(
			service.ProtectedEndpointInventoryIncomplete,
			"protected session state could not be verified",
			true, nil, probeErr,
		)
	}
	if s.beforeCAS != nil {
		s.beforeCAS()
	}
	changed, err := config.CompareAndSwapProjectAt(s.home, registration, nil)
	if err != nil {
		return result, projectRemovalInternal(err)
	}
	if !changed {
		return result, projectRemovalError(
			service.RegistrationChanged,
			"the project registration changed before it could be removed",
			true, nil, nil,
		)
	}
	project := registration.Persisted
	project.Repository = identity
	return ProjectRemovalResult{Project: project}, nil
}

func findExactProjectRegistration(
	projects []config.ProjectRegistration,
	path string,
) (config.ProjectRegistration, int) {
	var match config.ProjectRegistration
	matches := 0
	for _, project := range projects {
		if project.Persisted.Path == path {
			match = project
			matches++
		}
	}
	return match, matches
}

func (s *projectRemovalService) loadProtectedEndpoints(
	ctx context.Context,
	registration config.ProjectRegistration,
	identity string,
) ([]protectedEndpoint, error) {
	records := make(map[string]pullrequest.Provenance)
	err := pullrequest.NewFileStore(filepath.Join(s.home, "pull-requests.json")).View(
		ctx,
		func(current map[string]pullrequest.Provenance) error {
			for key, record := range current {
				records[key] = record
			}
			return nil
		},
	)
	if err != nil {
		retryable := errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) ||
			strings.Contains(err.Error(), "lock pull-request provenance")
		return nil, projectRemovalError(
			service.ProtectedEndpointInventoryIncomplete,
			"protected endpoint authority could not be read",
			retryable, nil, err,
		)
	}
	endpoints := make([]protectedEndpoint, 0)
	seen := make(map[string]protectedEndpoint)
	for _, record := range records {
		recordIdentity, identityErr := validateStableProjectIdentity(record.Project.Identity)
		if identityErr != nil {
			if record.Project.Path != "" &&
				utils.PathKey(record.Project.Path) == utils.PathKey(registration.Effective.Path) {
				return nil, incompleteProtectedAuthority(identityErr)
			}
			continue
		}
		if recordIdentity != identity {
			continue
		}
		if record.Project.Path == "" {
			return nil, incompleteProtectedAuthority(fmt.Errorf("project path is missing"))
		}
		if utils.PathKey(record.Project.Path) != utils.PathKey(registration.Effective.Path) {
			continue
		}
		workspace := record.Workspace
		if workspace.Path == "" || workspace.SessionName == "" ||
			git.ValidateWorktreeGeneration(workspace.Generation) != nil {
			return nil, incompleteProtectedAuthority(fmt.Errorf("protected endpoint record is incomplete"))
		}
		endpoint := protectedEndpoint{
			SessionName: workspace.SessionName,
			SocketName: tmux.ProtectedWorkspaceSocketName(
				workspace.SessionName,
				workspace.Path,
			),
			Generation: workspace.Generation,
		}
		key := endpoint.SocketName + "\x00" + endpoint.SessionName
		if previous, ok := seen[key]; ok && previous.Generation != endpoint.Generation {
			return nil, incompleteProtectedAuthority(fmt.Errorf("protected endpoint authority is ambiguous"))
		}
		if _, ok := seen[key]; !ok {
			seen[key] = endpoint
			endpoints = append(endpoints, endpoint)
		}
	}
	return endpoints, nil
}

func incompleteProtectedAuthority(cause error) error {
	return projectRemovalError(
		service.ProtectedEndpointInventoryIncomplete,
		"protected endpoint authority is incomplete",
		false, nil, cause,
	)
}

func classifyProjectRemovalFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return projectRemovalError(
			service.ProtectedEndpointInventoryIncomplete,
			"project removal could not acquire endpoint authority",
			true, nil, err,
		)
	}
	return projectRemovalInternal(err)
}

func projectRemovalInternal(cause error) error {
	return projectRemovalError(service.Internal, "internal failure", false, nil, cause)
}

func projectRemovalError(
	code service.Code,
	message string,
	retryable bool,
	details map[string]any,
	cause error,
) error {
	return service.NewError(code, message, retryable, details, cause)
}
