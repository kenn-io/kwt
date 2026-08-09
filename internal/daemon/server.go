package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
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

type ServerOptions struct {
	Token        string
	ExpectedHost string
	Status       StatusProvider
	Shutdown     ShutdownFunc
	Ping         http.Handler
	Touch        func(time.Time)
	Now          func() time.Time
	MaxBodyBytes int64
	Inventory    kwt.Inventory
	Remover      kwt.Remover
	Gate         *Gate
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

const defaultMaxBodyBytes = 1 << 20

func NewServer(opts ServerOptions) http.Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	mux := http.NewServeMux()
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
					return nil, problemFromError(err)
				}
				defer release()
				result, err := opts.Inventory.Query(ctx, input.Body)
				if err != nil {
					return nil, problemFromError(err)
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
					return nil, problemFromError(err)
				}
				defer release()
				if err := opts.Inventory.ApproveConfig(ctx, input.Body); err != nil {
					return nil, problemFromError(err)
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
					return nil, problemFromError(err)
				}
				defer release()
				result, err := opts.Remover.Remove(ctx, input.Body)
				if err != nil {
					return nil, problemFromError(err)
				}
				return &removalOutput{Body: result}, nil
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
				return nil, problemFromError(err)
			}
			return &shutdownOutput{Body: ShutdownResponse{Status: status}}, nil
		},
	)
	if opts.Ping != nil {
		mux.Handle("/api/ping", opts.Ping)
	}
	return secureLocalHandler(mux, opts)
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
			writeProblem(w, Problem{
				Type:   "https://kwt.dev/problems/invalid-request",
				Title:  http.StatusText(http.StatusBadRequest),
				Status: http.StatusBadRequest,
				Detail: "request host does not match the daemon endpoint",
				Code:   string(service.InvalidRequest),
			})
			return
		}
		if r.Header.Get("Origin") != "" {
			w.Header().Set("Connection", "close")
			writeProblem(w, Problem{
				Type:   "https://kwt.dev/problems/permission-denied",
				Title:  http.StatusText(http.StatusForbidden),
				Status: http.StatusForbidden,
				Detail: "browser-origin requests are not accepted",
				Code:   string(service.PermissionDenied),
			})
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
			writeProblem(w, Problem{
				Type:   "https://kwt.dev/problems/permission-denied",
				Title:  http.StatusText(http.StatusUnauthorized),
				Status: http.StatusUnauthorized,
				Detail: "a valid daemon bearer token is required",
				Code:   string(service.PermissionDenied),
			})
			return
		}
		if r.ContentLength > opts.MaxBodyBytes {
			writeProblem(w, Problem{
				Type:   "https://kwt.dev/problems/invalid-request",
				Title:  http.StatusText(http.StatusRequestEntityTooLarge),
				Status: http.StatusRequestEntityTooLarge,
				Detail: "request body exceeds the daemon limit",
				Code:   string(service.InvalidRequest),
			})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, opts.MaxBodyBytes)
		now := opts.Now()
		status := opts.Status.Status(now)
		shutdownPath := r.URL.Path == "/api/v1/daemon/shutdown"
		if opts.Touch != nil && (!shutdownPath || status.State != StateDraining) {
			opts.Touch(now)
		}
		if status.State == StateDraining &&
			r.URL.Path != "/api/v1/status" && !shutdownPath {
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
			writeProblem(w, Problem{
				Type:          "https://kwt.dev/problems/daemon-draining",
				Title:         "Daemon draining",
				Status:        http.StatusServiceUnavailable,
				Detail:        "the kwt daemon is draining for replacement or shutdown",
				Code:          "daemon_draining",
				Retryable:     true,
				DrainDeadline: status.DrainDeadline,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeProblem(w http.ResponseWriter, problem Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(problem.Status)
	_ = json.NewEncoder(w).Encode(problem)
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
	typed := service.AsError(err)
	status := http.StatusInternalServerError
	switch typed.Code {
	case service.InvalidRequest:
		status = http.StatusBadRequest
	case service.PermissionDenied:
		status = http.StatusForbidden
	case service.NotFound:
		status = http.StatusNotFound
	case service.Conflict, service.ConnectionChanged:
		status = http.StatusConflict
	case service.InteractionRequired:
		status = http.StatusPreconditionRequired
	case service.Busy:
		status = http.StatusServiceUnavailable
	case service.Unsupported:
		status = http.StatusNotImplemented
	case service.TransportFailure:
		status = http.StatusBadGateway
	}
	problem := &Problem{
		Type:      "https://kwt.dev/problems/" + string(typed.Code),
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    typed.Message,
		Code:      string(typed.Code),
		Retryable: typed.Retryable,
		Details:   allowedProblemDetails(typed.Details),
	}
	if deadline, ok := typed.Details["drain_deadline"].(time.Time); ok {
		problem.DrainDeadline = &deadline
	}
	if deadline, ok := typed.Details["drain_deadline"].(*time.Time); ok {
		problem.DrainDeadline = deadline
	}
	return problem
}

func allowedProblemDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	allowed := map[string]bool{
		"kind": true, "path": true, "digest": true, "size": true,
		"preview": true, "truncated": true, "drain_deadline": true,
		"branch": true, "reason": true, "worktree_removed": true,
		"branch_deleted": true, "registry_unregistered": true,
	}
	result := make(map[string]any)
	for key, value := range details {
		if allowed[key] {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
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
		return &Problem{
			Type:      "https://kwt.dev/problems/" + code,
			Title:     http.StatusText(status),
			Status:    status,
			Detail:    message,
			Code:      code,
			Retryable: status == http.StatusServiceUnavailable,
		}
	}
}
