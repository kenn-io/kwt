package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

type StatusProvider interface {
	Status(time.Time) Status
}

type ShutdownFunc func(context.Context, ShutdownRequest) (Status, error)

type SSHResolver interface {
	Resolve(context.Context, kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error)
}

type SSHLifecycle interface {
	Acquire(context.Context, kwt.SSHLeaseRequest) (kwt.SSHLease, error)
}

type ServerOptions struct {
	Token           string
	ExpectedHost    string
	Status          StatusProvider
	Shutdown        ShutdownFunc
	Ping            http.Handler
	Touch           func(time.Time)
	Now             func() time.Time
	MaxBodyBytes    int64
	Inventory       kwt.Inventory
	Remover         kwt.Remover
	ProjectRemover  kwt.ProjectRemover
	Operations      *OperationHub
	SSHResolver     SSHResolver
	SSHLifecycle    SSHLifecycle
	SSHLeaseTimeout time.Duration
	SSHLeases       *sshLeaseRegistry
	Gate            *Gate
	ReportError     func(string, *service.Error, kwt.ExpansionContext)
}

type emptyInput struct{}
type statusOutput struct{ Body Status }
type shutdownInput struct{ Body ShutdownRequest }
type shutdownOutput struct{ Body ShutdownResponse }
type inventoryInput struct{ Body kwt.Request }
type inventoryOutput struct{ Body kwt.Result }
type configApprovalInput struct{ Body kwt.ConfigApproval }
type configApprovalOutput struct {
	Body struct {
		Status string `json:"status"`
	}
}
type removalInput struct{ Body kwt.RemovalRequest }
type removalOutput struct{ Body kwt.RemovalResult }
type projectRemovalInput struct{ Body kwt.ProjectRemovalRequest }
type projectRemovalOutput struct{ Body kwt.ProjectRemovalResult }
type sshResolveInput struct{ Body kwt.SSHResolveRequest }
type sshResolveOutput struct{ Body kwt.SSHRouteSnapshot }

const defaultMaxBodyBytes = 1 << 20

func NewServer(opts ServerOptions) http.Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	mux := http.NewServeMux()
	registerOperationRoutes(mux, opts.Operations, opts)
	registerSSHLifecycleRoutes(mux, opts)
	config := huma.DefaultConfig("kwt daemon API", APISchemaVersion)
	config.OpenAPIPath = "/openapi"
	config.DocsPath = ""
	api := humago.New(mux, config)
	huma.Register(
		api,
		huma.Operation{
			Method:      http.MethodGet,
			Path:        "/api/v1/status",
			OperationID: "daemon-status",
		},
		func(_ context.Context, _ *emptyInput) (*statusOutput, error) {
			return &statusOutput{Body: opts.Status.Status(opts.Now())}, nil
		},
	)
	if opts.Inventory != nil {
		huma.Register(
			api,
			huma.Operation{Method: http.MethodPost, Path: "/api/v1/inventory", OperationID: "worktree-inventory"},
			func(ctx context.Context, input *inventoryInput) (*inventoryOutput, error) {
				release, err := reserveInventoryWork(opts)
				if err != nil {
					return nil, reportProblem(opts, "/api/v1/inventory", err)
				}
				defer release()
				result, err := opts.Inventory.Query(ctx, input.Body)
				if err != nil {
					return nil, reportProblemWithExpansion(
						opts, "/api/v1/inventory", err, input.Body.Expansion,
					)
				}
				return &inventoryOutput{Body: result}, nil
			},
		)
		huma.Register(
			api,
			huma.Operation{Method: http.MethodPost, Path: "/api/v1/config/trust", OperationID: "repository-config-trust"},
			func(ctx context.Context, input *configApprovalInput) (*configApprovalOutput, error) {
				release, err := reserveInventoryWork(opts)
				if err != nil {
					return nil, reportProblem(opts, "/api/v1/config/trust", err)
				}
				defer release()
				if err := opts.Inventory.ApproveConfig(ctx, input.Body); err != nil {
					return nil, reportProblem(opts, "/api/v1/config/trust", err)
				}
				output := &configApprovalOutput{}
				output.Body.Status = "approved"
				return output, nil
			},
		)
	}
	if opts.Remover != nil {
		huma.Register(
			api,
			huma.Operation{
				Method: http.MethodPost, Path: "/api/v1/worktrees/remove",
				OperationID: "worktree-remove",
			},
			func(ctx context.Context, input *removalInput) (*removalOutput, error) {
				release, err := reserveInventoryWork(opts)
				if err != nil {
					return nil, reportProblemWithExpansion(
						opts, "/api/v1/worktrees/remove", err, input.Body.Expansion,
					)
				}
				defer release()
				result, err := opts.Remover.Remove(ctx, input.Body)
				if err != nil {
					return nil, reportProblemWithExpansion(
						opts, "/api/v1/worktrees/remove", err, input.Body.Expansion,
					)
				}
				return &removalOutput{Body: result}, nil
			},
		)
	}
	if opts.ProjectRemover != nil {
		huma.Register(
			api,
			huma.Operation{
				Method: http.MethodPost, Path: "/api/v1/projects/remove",
				OperationID: "project-remove",
			},
			func(ctx context.Context, input *projectRemovalInput) (*projectRemovalOutput, error) {
				release, err := reserveInventoryWork(opts)
				if err != nil {
					return nil, reportProblem(opts, "/api/v1/projects/remove", err)
				}
				defer release()
				result, err := opts.ProjectRemover.RemoveProject(ctx, input.Body)
				if err != nil {
					return nil, reportProblemWithExpansion(
						opts, "/api/v1/projects/remove", err, input.Body.Expansion,
					)
				}
				return &projectRemovalOutput{Body: result}, nil
			},
		)
	}
	if opts.SSHResolver != nil {
		huma.Register(
			api,
			huma.Operation{
				Method: http.MethodPost, Path: "/api/v1/ssh/resolve",
				OperationID: "ssh-resolve",
			},
			func(ctx context.Context, input *sshResolveInput) (*sshResolveOutput, error) {
				if input.Body.Environment == nil || !filepath.IsAbs(input.Body.WorkingDirectory) {
					return nil, reportProblem(opts, "/api/v1/ssh/resolve", service.NewError(
						service.InvalidRequest,
						"SSH resolution requires an invocation environment and working directory",
						false,
						nil,
						nil,
					))
				}
				release, err := reserveInventoryWork(opts)
				if err != nil {
					return nil, reportProblem(opts, "/api/v1/ssh/resolve", err)
				}
				defer release()
				result, err := opts.SSHResolver.Resolve(ctx, input.Body)
				if err != nil {
					return nil, reportProblem(opts, "/api/v1/ssh/resolve", err)
				}
				if err := validateSSHSnapshotSize(result); err != nil {
					return nil, reportProblem(opts, "/api/v1/ssh/resolve", err)
				}
				return &sshResolveOutput{Body: result}, nil
			},
		)
	}
	huma.Register(
		api,
		huma.Operation{
			Method:      http.MethodPost,
			Path:        "/api/v1/daemon/shutdown",
			OperationID: "daemon-shutdown",
		},
		func(ctx context.Context, input *shutdownInput) (*shutdownOutput, error) {
			status, err := opts.Shutdown(ctx, input.Body)
			if err != nil {
				return nil, reportProblem(opts, "/api/v1/daemon/shutdown", err)
			}
			return &shutdownOutput{Body: ShutdownResponse{Status: status}}, nil
		},
	)
	if opts.Ping != nil {
		mux.Handle("/api/ping", opts.Ping)
	}
	return secureLocalHandler(mux, opts)
}

func validateSSHSnapshotSize(snapshot kwt.SSHRouteSnapshot) error {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return service.NewError(service.Internal, "internal failure", false, nil, err)
	}
	if int64(len(encoded)) > sshSnapshotLimit {
		return service.NewError(
			service.SSHResolutionFailed,
			"SSH route snapshot exceeds the supported size limit",
			false,
			nil,
			nil,
		)
	}
	return nil
}

func reserveInventoryWork(opts ServerOptions) (func(), error) {
	if opts.Gate == nil {
		return func() {}, nil
	}
	return opts.Gate.Reserve(ReservationWork, opts.Now())
}

func secureLocalHandler(next http.Handler, opts ServerOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != opts.ExpectedHost {
			w.Header().Set("Connection", "close")
			writeProblem(w, newProblem(http.StatusBadRequest, service.Descriptor{
				Code: service.InvalidRequest, Message: "request host does not match the daemon endpoint",
			}))
			return
		}
		if r.Header.Get("Origin") != "" {
			w.Header().Set("Connection", "close")
			writeProblem(w, newProblem(http.StatusForbidden, service.Descriptor{
				Code: service.PermissionDenied, Message: "browser-origin requests are not accepted",
			}))
			return
		}
		if r.URL.Path == "/api/ping" || r.URL.Path == "/openapi.json" {
			next.ServeHTTP(w, r)
			return
		}
		authorizations := r.Header.Values("Authorization")
		expectedAuthorization := "Bearer " + opts.Token
		if len(authorizations) != 1 || subtle.ConstantTimeCompare(
			[]byte(authorizations[0]),
			[]byte(expectedAuthorization),
		) != 1 {
			w.Header().Set("Connection", "close")
			writeProblem(w, newProblem(http.StatusUnauthorized, service.Descriptor{
				Code: service.PermissionDenied, Message: "a valid daemon bearer token is required",
			}))
			return
		}
		if r.ContentLength > opts.MaxBodyBytes {
			writeProblem(w, newProblem(http.StatusRequestEntityTooLarge, service.Descriptor{
				Code: service.InvalidRequest, Message: "request body exceeds the daemon limit",
			}))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, opts.MaxBodyBytes)
		now := opts.Now()
		status := opts.Status.Status(now)
		shutdownPath := r.URL.Path == "/api/v1/daemon/shutdown"
		operationPath := strings.HasPrefix(r.URL.Path, operationRoutePrefix)
		leaseControlPath := drainingSSHLeaseControl(r.Method, r.URL.Path)
		if opts.Touch != nil && (!shutdownPath || status.State != StateDraining) {
			opts.Touch(now)
		}
		if status.State == StateDraining &&
			r.URL.Path != "/api/v1/status" && !shutdownPath && !operationPath && !leaseControlPath {
			if status.DrainDeadline != nil {
				retryAfter := status.DrainDeadline.Sub(now)
				seconds := int64(retryAfter / time.Second)
				if retryAfter%time.Second != 0 {
					seconds++
				}
				if seconds < 0 {
					seconds = 0
				}
				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			}
			details := make(map[string]any)
			if status.DrainDeadline != nil {
				details["drain_deadline"] = status.DrainDeadline.Format(time.RFC3339Nano)
			}
			writeProblem(w, newProblem(http.StatusServiceUnavailable, service.Descriptor{
				Code:      service.DaemonDraining,
				Message:   "the kwt daemon is draining for replacement or shutdown",
				Retryable: true,
				Details:   details,
			}))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func drainingSSHLeaseControl(method, path string) bool {
	const prefix = "/api/v1/ssh/leases/"
	remainder, ok := strings.CutPrefix(path, prefix)
	if !ok || remainder == "" {
		return false
	}
	if method == http.MethodDelete {
		return !strings.Contains(remainder, "/")
	}
	if method != http.MethodPost {
		return false
	}
	leaseID, suffix, ok := strings.Cut(remainder, "/")
	return ok && leaseID != "" && suffix == "touch"
}

func writeProblem(w http.ResponseWriter, problem Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(problem.Status)
	_ = json.NewEncoder(w).Encode(problem)
}

func newProblem(status int, descriptor service.Descriptor) Problem {
	problem := Problem{
		Type:  "https://kwt.dev/problems/" + string(descriptor.Code),
		Title: http.StatusText(status), Status: status, Detail: descriptor.Message,
		Descriptor: descriptor,
	}
	if deadline, ok := drainDeadlineDetail(descriptor.Details); ok {
		problem.DrainDeadline = &deadline
	}
	return problem
}

func drainDeadlineDetail(details map[string]any) (time.Time, bool) {
	switch deadline := details["drain_deadline"].(type) {
	case time.Time:
		return deadline, true
	case *time.Time:
		if deadline != nil {
			return *deadline, true
		}
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, deadline)
		return parsed, err == nil
	}
	return time.Time{}, false
}

func (p *Problem) Error() string { return p.Detail }

func (p *Problem) GetStatus() int { return p.Status }

func (p *Problem) ContentType(value string) string {
	if value == "application/json" {
		return "application/problem+json"
	}
	return value
}

func problemFromError(err error) *Problem {
	descriptor := publicErrorDescriptor(err)
	status := http.StatusInternalServerError
	switch descriptor.Code {
	case service.InvalidRequest, service.SSHInvalidTarget:
		status = http.StatusBadRequest
	case service.PermissionDenied:
		status = http.StatusForbidden
	case service.NotFound, service.ProjectNotFound:
		status = http.StatusNotFound
	case service.Conflict, service.ConnectionChanged, service.OperationIDConflict,
		service.RegistrationChanged, service.ProtectedSessionLive,
		service.SSHRouteUnreviewable, service.SSHConfigurationChanged,
		service.SSHPromptRejected, service.SSHConnectionChanged,
		service.SSHControlPathOccupied:
		status = http.StatusConflict
	case service.ProtectedEndpointInventoryIncomplete:
		if descriptor.Retryable {
			status = http.StatusServiceUnavailable
		} else {
			status = http.StatusConflict
		}
	case service.DaemonDowngradeRefused, service.DaemonBuildOrderUnknown:
		status = http.StatusConflict
	case service.InteractionRequired, service.SSHInteractionRequired:
		status = http.StatusPreconditionRequired
	case service.Busy, service.DaemonStartFailed, service.DaemonUnresponsive,
		service.DaemonDraining, service.InventoryTimeout,
		service.OperationCapacityExhausted, service.SSHResolutionFailed,
		service.SSHConnectionFailed, service.SSHCleanupFailed:
		status = http.StatusServiceUnavailable
	case service.SSHPromptTimedOut:
		status = http.StatusRequestTimeout
	case service.OperationOutcomeUnknown:
		status = http.StatusConflict
	case service.Unsupported, service.SSHUnsupportedVersion:
		status = http.StatusNotImplemented
	case service.DaemonIncompatible:
		status = http.StatusUpgradeRequired
	case service.TransportFailure, service.DaemonTransportFailed:
		status = http.StatusBadGateway
	}
	problem := newProblem(status, descriptor)
	return &problem
}

func publicErrorDescriptor(err error) service.Descriptor {
	typed := service.AsError(err)
	message := typed.Message
	retryable := typed.Retryable
	if typed.Code == service.Internal {
		message = "internal failure"
	}
	if typed.Code == service.OperationOutcomeUnknown {
		retryable = false
	}
	return service.Descriptor{
		Code: typed.Code, Message: message, Retryable: retryable,
		Details: allowedProblemDetails(typed.Code, typed.Details),
	}
}

func reportProblem(opts ServerOptions, route string, err error) *Problem {
	return reportProblemWithExpansion(opts, route, err, kwt.ExpansionContext{})
}

func reportProblemWithExpansion(
	opts ServerOptions,
	route string,
	err error,
	expansion kwt.ExpansionContext,
) *Problem {
	typed := service.AsError(err)
	if opts.ReportError != nil {
		opts.ReportError(route, typed, expansion)
	}
	return problemFromError(typed)
}

type problemDetailType uint8

const (
	detailString problemDetailType = iota + 1
	detailBool
	detailNumber
	detailRFC3339
)

var allowedProblemDetailTypes = map[service.Code]map[string]problemDetailType{
	service.DaemonDraining: {"drain_deadline": detailRFC3339},
	service.InteractionRequired: {
		"kind": detailString, "path": detailString, "digest": detailString,
		"size": detailNumber, "preview": detailString, "truncated": detailBool,
	},
	service.Conflict: {
		"path": detailString, "branch": detailString, "reason": detailString,
		"worktree_removed": detailBool, "branch_deleted": detailBool,
		"registry_unregistered": detailBool,
	},
	service.ConnectionChanged: {
		"path": detailString, "branch": detailString, "reason": detailString,
		"worktree_removed": detailBool, "branch_deleted": detailBool,
		"registry_unregistered": detailBool,
	},
	service.Busy: {
		"path": detailString, "branch": detailString, "reason": detailString,
		"worktree_removed": detailBool, "branch_deleted": detailBool,
		"registry_unregistered": detailBool,
	},
	service.Internal: {
		"path": detailString, "branch": detailString, "worktree_removed": detailBool,
		"branch_deleted": detailBool, "registry_unregistered": detailBool,
	},
	service.RemovalFailed: {
		"path": detailString, "branch": detailString, "worktree_removed": detailBool,
		"branch_deleted": detailBool, "registry_unregistered": detailBool,
	},
	service.ProtectedSessionLive: {
		"session_name": detailString, "socket_name": detailString,
		"generation": detailString,
	},
}

func allowedProblemDetails(code service.Code, details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	allowed := allowedProblemDetailTypes[code]
	if len(allowed) == 0 {
		return nil
	}
	result := make(map[string]any)
	for key, value := range details {
		typeOf, ok := allowed[key]
		if !ok {
			continue
		}
		if normalized, ok := normalizeProblemDetail(typeOf, value); ok {
			result[key] = normalized
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func normalizeProblemDetail(kind problemDetailType, value any) (any, bool) {
	switch kind {
	case detailString:
		result, ok := value.(string)
		return result, ok
	case detailBool:
		result, ok := value.(bool)
		return result, ok
	case detailNumber:
		switch value.(type) {
		case int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64, float32, float64:
			return value, true
		default:
			return nil, false
		}
	case detailRFC3339:
		switch result := value.(type) {
		case time.Time:
			return result.Format(time.RFC3339Nano), true
		case *time.Time:
			if result != nil {
				return result.Format(time.RFC3339Nano), true
			}
		case string:
			if _, err := time.Parse(time.RFC3339Nano, result); err == nil {
				return result, true
			}
		}
	}
	return nil, false
}

func init() {
	huma.NewError = func(status int, message string, _ ...error) huma.StatusError {
		code := "internal"
		switch status {
		case http.StatusBadRequest,
			http.StatusRequestEntityTooLarge,
			http.StatusUnprocessableEntity:
			code = "invalid_request"
		case http.StatusUnauthorized, http.StatusForbidden:
			code = "permission_denied"
		case http.StatusNotFound:
			code = "not_found"
		case http.StatusConflict:
			code = "conflict"
		case http.StatusServiceUnavailable:
			code = "busy"
		}
		returnProblem := newProblem(status, service.Descriptor{
			Code: service.Code(code), Message: message,
			Retryable: status == http.StatusServiceUnavailable,
		})
		return &returnProblem
	}
}
