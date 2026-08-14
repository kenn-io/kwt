package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

type Client struct {
	endpoint      kitdaemon.Endpoint
	token         string
	capabilities  []string
	controlHTTP   *http.Client
	inventoryHTTP *http.Client
	mutationHTTP  *http.Client
	sshHTTP       *http.Client
	streamHTTP    *http.Client
	operationHTTP *http.Client
}

type worktreeRemovedError struct {
	err error
}

type refreshRequiredError struct {
	err error
}

type removalRequestV1 struct {
	RepositoryPath     string `json:"repository_path"`
	Path               string `json:"path"`
	ExpectedGeneration string `json:"expected_generation"`
	Force              bool   `json:"force,omitempty"`
	DeleteBranch       bool   `json:"delete_branch,omitempty"`
	ForceDeleteBranch  bool   `json:"force_delete_branch,omitempty"`
}

func (e *worktreeRemovedError) Error() string {
	return e.err.Error()
}

func (e *worktreeRemovedError) Unwrap() error {
	return e.err
}

func (e *worktreeRemovedError) WorktreeRemoved() bool {
	return true
}

func (e *refreshRequiredError) Error() string {
	return e.err.Error()
}

func (e *refreshRequiredError) Unwrap() error {
	return e.err
}

func (e *refreshRequiredError) RefreshRequired() bool {
	return true
}

// RequiresRefresh reports whether a mutation result could not be reconciled
// after its response was lost. Callers should refresh observable state without
// assuming that the mutation completed.
func RequiresRefresh(err error) bool {
	var required interface{ RefreshRequired() bool }
	return errors.As(err, &required) && required.RefreshRequired()
}

var ErrResponseTooLarge = errors.New("kwt daemon response is too large")

const (
	controlRequestTimeout              = 2 * time.Second
	inventoryResponseHeadroom          = 5 * time.Second
	inventoryRequestTimeout            = kwt.DefaultRefreshTimeout + inventoryResponseHeadroom
	sshResolveRequestTimeout           = 30 * time.Second
	operationStreamHeaderTimeout       = 2 * time.Second
	operationResponseTimeout           = 2 * time.Second
	removalReconcileTimeout            = 5 * time.Second
	controlResponseLimit         int64 = 1 << 20
	inventoryResponseLimit             = 64 << 20
	sshSnapshotLimit                   = 8 << 20
	sshResponseLimit                   = sshSnapshotLimit + (64 << 10)
)

func NewVerifiedClient(
	ctx context.Context,
	rec kitdaemon.RuntimeRecord,
	token string,
) (*Client, error) {
	if token == "" {
		return nil, errors.New("daemon bearer token is empty")
	}
	proof, err := kitdaemon.NewProof([]byte(token))
	if err != nil {
		return nil, err
	}
	info, err := proof.Probe(ctx, rec, kitdaemon.ProbeOptions{
		ExpectedService: ServiceName,
		Timeout:         time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("prove kwt daemon endpoint: %w", err)
	}
	if info.PID != rec.PID {
		return nil, errors.New("daemon PID does not match runtime record")
	}
	ep := rec.Endpoint()
	return &Client{
		endpoint: ep,
		token:    token,
		controlHTTP: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			Timeout:               controlRequestTimeout,
			ResponseHeaderTimeout: controlRequestTimeout,
		}),
		inventoryHTTP: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			Timeout:               inventoryRequestTimeout,
			ResponseHeaderTimeout: inventoryRequestTimeout,
		}),
		mutationHTTP: ep.HTTPClient(kitdaemon.HTTPClientOptions{}),
		sshHTTP: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			Timeout:               sshResolveRequestTimeout,
			ResponseHeaderTimeout: sshResolveRequestTimeout,
		}),
		streamHTTP: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			ResponseHeaderTimeout: operationStreamHeaderTimeout,
		}),
		operationHTTP: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			Timeout:               operationResponseTimeout,
			ResponseHeaderTimeout: operationResponseTimeout,
		}),
	}, nil
}

func (c *Client) Inventory(ctx context.Context, request kwt.Request) (kwt.Result, error) {
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return kwt.Result{}, err
	}
	request.Expansion = expansion
	return c.inventory(ctx, request)
}

func (c *Client) inventory(ctx context.Context, request kwt.Request) (kwt.Result, error) {
	var result kwt.Result
	err := c.doWith(
		ctx,
		c.inventoryHTTP,
		inventoryResponseLimit,
		http.MethodPost,
		"/api/v1/inventory",
		request,
		&result,
	)
	return result, err
}

func (c *Client) ApproveConfig(ctx context.Context, approval kwt.ConfigApproval) error {
	return c.do(ctx, http.MethodPost, "/api/v1/config/trust", approval, nil)
}

var captureRemovalExpansionContext = kwt.CaptureExpansionContext

func (c *Client) ResolveSSH(
	ctx context.Context,
	request kwt.SSHResolveRequest,
) (kwt.SSHRouteSnapshot, error) {
	snapshot, err := config.LoadGlobalSnapshot()
	if err != nil {
		return kwt.SSHRouteSnapshot{}, err
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return kwt.SSHRouteSnapshot{}, err
	}
	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return kwt.SSHRouteSnapshot{}, err
	}
	request.WorkingDirectory = workingDirectory
	request.Environment = credentials.StripEnvironment(
		os.Environ(),
		credentials.ProtectedNames(snapshot.Config),
	)
	var result kwt.SSHRouteSnapshot
	err = c.doWith(
		ctx,
		c.sshHTTP,
		sshResponseLimit,
		http.MethodPost,
		"/api/v1/ssh/resolve",
		request,
		&result,
	)
	return result, err
}

func (c *Client) AcquireSSH(
	ctx context.Context,
	request kwt.SSHLeaseRequest,
	callbacks OperationCallbacks,
) (SSHLeaseResult, error) {
	missingPromptHandler := errors.New("SSH prompt handler is unavailable")
	if callbacks.Prompt == nil {
		callbacks.Prompt = func(context.Context, service.OperationPrompt) (string, error) {
			return "", missingPromptHandler
		}
	}
	snapshot, err := config.LoadGlobalSnapshot()
	if err != nil {
		return SSHLeaseResult{}, err
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return SSHLeaseResult{}, err
	}
	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return SSHLeaseResult{}, err
	}
	request.WorkingDirectory = workingDirectory
	request.Environment = credentials.StripEnvironment(
		os.Environ(),
		credentials.ProtectedNames(snapshot.Config),
	)
	operationID, err := randomOperationID()
	if err != nil {
		return SSHLeaseResult{}, err
	}
	accepted := SSHLeaseOperation{OperationID: operationID}
	startErr := c.doWith(
		ctx,
		c.operationHTTP,
		controlResponseLimit,
		http.MethodPost,
		sshLeaseRoute,
		SSHLeaseOperationRequest{OperationID: operationID, Lease: request},
		&accepted,
	)
	if startErr != nil && !service.IsCode(startErr, service.DaemonTransportFailed) {
		return SSHLeaseResult{}, startErr
	}
	raw, followErr := c.FollowOperation(ctx, operationID, 0, callbacks)
	var result SSHLeaseResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return SSHLeaseResult{}, errors.Join(followErr, service.NewError(
				service.Internal,
				"internal failure",
				false,
				nil,
				err,
			))
		}
	}
	if errors.Is(followErr, missingPromptHandler) {
		return result, service.NewError(
			service.SSHInteractionRequired,
			"SSH interaction is required",
			false,
			nil,
			errors.Join(followErr, startErr),
		)
	}
	if followErr != nil {
		return result, errors.Join(followErr, startErr)
	}
	if len(raw) == 0 {
		return SSHLeaseResult{}, service.NewError(
			service.Internal,
			"internal failure",
			false,
			nil,
			errors.New("SSH lease operation returned an empty result"),
		)
	}
	return result, nil
}

func (c *Client) TouchSSHLease(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return service.NewError(service.InvalidRequest, "SSH lease ID is empty", false, nil, nil)
	}
	return c.doWith(
		ctx, c.controlHTTP, controlResponseLimit,
		http.MethodPost, sshLeaseRoute+"/"+leaseID+"/touch", nil, nil,
	)
}

func (c *Client) ReleaseSSHLease(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return service.NewError(service.InvalidRequest, "SSH lease ID is empty", false, nil, nil)
	}
	return c.doWith(
		ctx, c.controlHTTP, controlResponseLimit,
		http.MethodDelete, sshLeaseRoute+"/"+leaseID, nil, nil,
	)
}

func (c *Client) RemoveWorktree(
	ctx context.Context,
	request kwt.RemovalRequest,
) (kwt.RemovalResult, error) {
	payload := any(removalRequestV1{
		RepositoryPath:     request.RepositoryPath,
		Path:               request.Path,
		ExpectedGeneration: request.ExpectedGeneration,
		Force:              request.Force,
		DeleteBranch:       request.DeleteBranch,
		ForceDeleteBranch:  request.ForceDeleteBranch,
	})
	if request.Session != nil || slices.Contains(c.capabilities, CapabilityGuardedRemoval) {
		expansion, expansionErr := captureRemovalExpansionContext()
		if expansionErr != nil && request.Session != nil {
			return kwt.RemovalResult{}, expansionErr
		}
		if expansionErr == nil {
			request.Expansion = expansion
		}
		payload = request
	}
	var result kwt.RemovalResult
	err := c.doWith(
		ctx,
		c.mutationHTTP,
		controlResponseLimit,
		http.MethodPost,
		"/api/v1/worktrees/remove",
		payload,
		&result,
	)
	if err != nil {
		result = removalResultFromError(err)
		if result.WorktreeRemoved {
			err = &worktreeRemovedError{err: err}
		} else if service.IsCode(err, service.DaemonTransportFailed) {
			removed, reconcileErr := c.reconcileRemoval(request)
			if reconcileErr != nil {
				err = &refreshRequiredError{err: errors.Join(
					err,
					fmt.Errorf("reconcile worktree removal: %w", reconcileErr),
				)}
			} else if removed {
				result = kwt.RemovalResult{
					Path:            request.Path,
					WorktreeRemoved: true,
				}
				err = &worktreeRemovedError{err: err}
			}
		}
	}
	return result, err
}

func (c *Client) RemoveProject(
	ctx context.Context,
	request kwt.ProjectRemovalRequest,
) (kwt.ProjectRemovalResult, error) {
	var result kwt.ProjectRemovalResult
	err := c.doWith(
		ctx,
		c.mutationHTTP,
		controlResponseLimit,
		http.MethodPost,
		"/api/v1/projects/remove",
		request,
		&result,
	)
	if service.IsCode(err, service.DaemonTransportFailed) {
		reconciled, completed, reconcileErr := c.reconcileProjectRemoval(request)
		switch {
		case reconcileErr != nil && service.IsCode(reconcileErr, service.RegistrationChanged):
			return kwt.ProjectRemovalResult{}, reconcileErr
		case reconcileErr != nil:
			err = &refreshRequiredError{err: errors.Join(
				err,
				fmt.Errorf("reconcile project removal: %w", reconcileErr),
			)}
		case completed:
			return reconciled, nil
		}
	}
	return result, err
}

func (c *Client) reconcileProjectRemoval(
	request kwt.ProjectRemovalRequest,
) (kwt.ProjectRemovalResult, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), removalReconcileTimeout)
	defer cancel()
	result, err := c.inventory(ctx, kwt.Request{
		View:            kwt.ViewProjects,
		RequireCurrent:  true,
		UntrustedConfig: kwt.IgnoreUntrustedConfig,
		Expansion:       request.Expansion,
	})
	if err != nil {
		return kwt.ProjectRemovalResult{}, false, err
	}
	matches := make([]kwt.Project, 0, 1)
	for _, project := range result.Snapshot.Projects {
		if project.Path == request.Path {
			matches = append(matches, project)
		}
	}
	if len(matches) == 0 {
		return kwt.ProjectRemovalResult{Project: models.Project{
			Path: request.Path, Repository: request.ExpectedRepository,
		}}, true, nil
	}
	if len(matches) != 1 {
		return kwt.ProjectRemovalResult{}, false, service.NewError(
			service.InventoryFailed,
			"project removal reconciliation is ambiguous",
			true,
			nil,
			nil,
		)
	}
	if !lifecycle.EqualProjectIdentity(
		matches[0].Repository, request.ExpectedRepository,
	) || matches[0].RegistrationFingerprint != request.ExpectedRegistration {
		return kwt.ProjectRemovalResult{}, false, service.NewError(
			service.RegistrationChanged,
			"the project registration changed while removal was being reconciled",
			true,
			nil,
			nil,
		)
	}
	return kwt.ProjectRemovalResult{}, false, nil
}

func (c *Client) reconcileRemoval(request kwt.RemovalRequest) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), removalReconcileTimeout)
	defer cancel()
	result, err := c.Inventory(ctx, kwt.Request{
		View:             kwt.ViewRepository,
		WorkingDirectory: request.RepositoryPath,
		RequireCurrent:   true,
		UntrustedConfig:  kwt.IgnoreUntrustedConfig,
	})
	if err != nil {
		return false, err
	}
	wantedPath := utils.PathKey(request.Path)
	for _, entry := range result.Snapshot.Entries {
		if utils.PathKey(entry.Path) == wantedPath &&
			entry.Generation == request.ExpectedGeneration {
			return false, nil
		}
	}
	return true, nil
}

func removalResultFromError(err error) kwt.RemovalResult {
	var typed *service.Error
	if !errors.As(err, &typed) {
		return kwt.RemovalResult{}
	}
	return kwt.RemovalResult{
		Path:                 detailStringValue(typed.Details, "path"),
		Branch:               detailStringValue(typed.Details, "branch"),
		WorktreeRemoved:      detailBoolValue(typed.Details, "worktree_removed"),
		BranchDeleted:        detailBoolValue(typed.Details, "branch_deleted"),
		RegistryUnregistered: detailBoolValue(typed.Details, "registry_unregistered"),
	}
}

func detailStringValue(details map[string]any, key string) string {
	value, _ := details[key].(string)
	return value
}

func detailBoolValue(details map[string]any, key string) bool {
	value, _ := details[key].(bool)
	return value
}

func newClient(ep kitdaemon.Endpoint, token string, client *http.Client) *Client {
	return &Client{
		endpoint: ep, token: token, controlHTTP: client, inventoryHTTP: client,
		mutationHTTP: client, sshHTTP: client, streamHTTP: client, operationHTTP: client,
	}
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var status Status
	err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, &status)
	return status, err
}

func (c *Client) Shutdown(
	ctx context.Context,
	reason string,
) (ShutdownResponse, error) {
	var output ShutdownResponse
	err := c.do(
		ctx,
		http.MethodPost,
		"/api/v1/daemon/shutdown",
		ShutdownRequest{Reason: reason},
		&output,
	)
	return output, err
}

func (c *Client) do(
	ctx context.Context,
	method string,
	path string,
	input any,
	output any,
) error {
	return c.doWith(
		ctx,
		c.controlHTTP,
		controlResponseLimit,
		method,
		path,
		input,
		output,
	)
}

func (c *Client) doWith(
	ctx context.Context,
	client *http.Client,
	responseLimit int64,
	method string,
	path string,
	input any,
	output any,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		c.endpoint.BaseURL()+path,
		body,
	)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return service.NewError(
			service.DaemonTransportFailed,
			"kwt daemon request failed",
			true,
			nil,
			err,
		)
	}
	defer func() { _ = resp.Body.Close() }()
	encoded, err := readResponse(resp.Body, responseLimit, path)
	if err != nil {
		message := "read kwt daemon response"
		if errors.Is(err, ErrResponseTooLarge) {
			message = err.Error()
		}
		return service.NewError(
			service.DaemonTransportFailed,
			message,
			true,
			nil,
			err,
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeProblem(resp.StatusCode, bytes.NewReader(encoded))
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(encoded, output); err != nil {
		return service.NewError(
			service.DaemonTransportFailed,
			"decode kwt daemon response",
			true,
			nil,
			err,
		)
	}
	return nil
}

func readResponse(body io.Reader, limit int64, path string) ([]byte, error) {
	encoded, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > limit {
		return nil, fmt.Errorf(
			"%w: %s exceeds %d bytes",
			ErrResponseTooLarge,
			path,
			limit,
		)
	}
	return encoded, nil
}

func decodeProblem(status int, body io.Reader) error {
	var problem Problem
	if err := json.NewDecoder(body).Decode(&problem); err != nil {
		return service.NewError(
			service.DaemonTransportFailed,
			fmt.Sprintf("kwt daemon returned HTTP %d", status),
			true,
			nil,
			err,
		)
	}
	code, ok := problemCode(problem.Code)
	if !ok {
		return service.NewError(
			service.DaemonTransportFailed,
			fmt.Sprintf("kwt daemon returned HTTP %d", status),
			true,
			nil,
			nil,
		)
	}
	message := problem.Message
	if message == "" {
		message = problem.Detail
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if code == service.Internal {
		message = "internal failure"
	}
	details := problem.Details
	if _, validDeadline := drainDeadlineDetail(details); problem.DrainDeadline != nil && !validDeadline {
		details = maps.Clone(details)
		if details == nil {
			details = make(map[string]any)
		}
		details["drain_deadline"] = *problem.DrainDeadline
	}
	return service.NewDescriptorError(service.Descriptor{
		Code: code, Message: message, Retryable: problem.Retryable,
		Details: details,
	}, nil)
}

func problemCode(code service.Code) (service.Code, bool) {
	switch code {
	case service.InvalidRequest,
		service.PermissionDenied,
		service.NotFound,
		service.Conflict,
		service.Busy,
		service.InteractionRequired,
		service.ConnectionChanged,
		service.Unsupported,
		service.TransportFailure,
		service.DaemonStartFailed,
		service.DaemonUnresponsive,
		service.DaemonIncompatible,
		service.DaemonDowngradeRefused,
		service.DaemonBuildOrderUnknown,
		service.DaemonDraining,
		service.DaemonTransportFailed,
		service.InventoryTimeout,
		service.InventoryFailed,
		service.RemovalFailed,
		service.ProjectNotFound,
		service.RegistrationChanged,
		service.UnregistrationFailed,
		service.ProtectedSessionLive,
		service.ProtectedEndpointInventoryIncomplete,
		service.OperationIDConflict,
		service.OperationCapacityExhausted,
		service.OperationJournalUnavailable,
		service.OperationOutcomeUnknown,
		service.SSHInvalidTarget,
		service.SSHResolutionFailed,
		service.SSHRouteUnreviewable,
		service.SSHConfigurationChanged,
		service.SSHUnsupportedVersion,
		service.SSHInteractionRequired,
		service.SSHPromptRejected,
		service.SSHPromptTimedOut,
		service.SSHConnectionFailed,
		service.SSHConnectionChanged,
		service.SSHControlPathOccupied,
		service.SSHCleanupFailed,
		service.Internal:
		return code, true
	default:
		return "", false
	}
}
