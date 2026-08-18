package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

const sshLeaseRoute = "/api/v1/ssh/leases"
const defaultSSHLeaseTimeout = 30 * time.Second
const defaultSSHCleanupTimeout = 5 * time.Second

type SSHLeaseOperationRequest struct {
	OperationID string              `json:"operation_id"`
	Lease       kwt.SSHLeaseRequest `json:"lease"`
}

type SSHLeaseOperation struct {
	OperationID string `json:"operation_id"`
}

type SSHLeaseResult struct {
	LeaseID       string           `json:"lease_id"`
	RouteIdentity string           `json:"route_identity"`
	Generation    uint64           `json:"generation"`
	Mode          kwt.SSHLeaseMode `json:"mode"`
	Arguments     []string         `json:"arguments"`
}

type sshLeaseRegistry struct {
	gate           *Gate
	now            func() time.Time
	ttl            time.Duration
	cleanupTimeout time.Duration

	mu     sync.Mutex
	leases map[string]*sshLeaseEntry
	closed bool
}

type sshLeaseEntry struct {
	lease       kwt.SSHLease
	reservation func()
	reserveOnce sync.Once

	mu          sync.Mutex
	released    bool
	cleaning    bool
	cleanupDone chan struct{}
	expires     time.Time
	timer       *time.Timer
	holds       int
}

func newSSHLeaseRegistry(gate *Gate, now func() time.Time, ttl time.Duration) *sshLeaseRegistry {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = defaultSSHLeaseTimeout
	}
	return &sshLeaseRegistry{
		gate: gate, now: now, ttl: ttl, cleanupTimeout: defaultSSHCleanupTimeout,
		leases: make(map[string]*sshLeaseEntry),
	}
}

func (r *sshLeaseRegistry) add(lease kwt.SSHLease) (string, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return "", service.NewError(service.DaemonDraining, "daemon is draining", true, nil, nil)
	}
	var reservation func()
	if r.gate != nil {
		var err error
		reservation, err = r.gate.Reserve(ReservationLease, r.now())
		if err != nil {
			return "", err
		}
	}
	for attempts := 0; attempts < 4; attempts++ {
		id, err := randomOperationID()
		if err != nil {
			if reservation != nil {
				reservation()
			}
			return "", err
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			if reservation != nil {
				reservation()
			}
			return "", service.NewError(service.DaemonDraining, "daemon is draining", true, nil, nil)
		}
		if _, exists := r.leases[id]; !exists {
			entry := &sshLeaseEntry{lease: lease, reservation: reservation}
			r.leases[id] = entry
			entry.mu.Lock()
			r.scheduleExpiry(id, entry)
			entry.mu.Unlock()
			r.mu.Unlock()
			return id, nil
		}
		r.mu.Unlock()
	}
	if reservation != nil {
		reservation()
	}
	return "", errors.New("generate unique SSH lease ID")
}

func (r *sshLeaseRegistry) close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.closed = true
	entries := make([]*sshLeaseEntry, 0, len(r.leases))
	for _, entry := range r.leases {
		entries = append(entries, entry)
	}
	clear(r.leases)
	r.mu.Unlock()
	for _, entry := range entries {
		entry.mu.Lock()
		if !entry.released {
			entry.released = true
			if entry.timer != nil {
				entry.timer.Stop()
				entry.timer = nil
			}
		}
		entry.mu.Unlock()
		entry.releaseReservation()
	}
	return nil
}

func (r *sshLeaseRegistry) scheduleExpiry(id string, entry *sshLeaseEntry) {
	entry.expires = r.now().Add(r.ttl)
	expires := entry.expires
	entry.timer = time.AfterFunc(r.ttl, func() {
		r.expire(id, entry, expires)
	})
}

func (r *sshLeaseRegistry) expire(id string, entry *sshLeaseEntry, expires time.Time) {
	entry.mu.Lock()
	if entry.released || entry.cleaning || entry.holds > 0 || !entry.expires.Equal(expires) {
		entry.mu.Unlock()
		return
	}
	r.beginCleanupLocked(entry)
	entry.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), r.cleanupTimeout)
	defer cancel()
	_ = r.cleanup(ctx, id, entry)
}

func (r *sshLeaseRegistry) entry(id string) (*sshLeaseEntry, error) {
	r.mu.Lock()
	entry := r.leases[id]
	r.mu.Unlock()
	if entry == nil {
		return nil, service.NewError(service.NotFound, "SSH lease was not found", false, nil, nil)
	}
	return entry, nil
}

func (r *sshLeaseRegistry) touch(id string) error {
	entry, err := r.entry(id)
	if err != nil {
		return err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.released || entry.cleaning {
		return service.NewError(service.NotFound, "SSH lease was not found", false, nil, nil)
	}
	if err := entry.lease.Touch(); err != nil {
		return err
	}
	if entry.holds == 0 {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		r.scheduleExpiry(id, entry)
	}
	if r.gate != nil {
		r.gate.Touch(r.now())
	}
	return nil
}

func (r *sshLeaseRegistry) hold(id string) (func(), error) {
	entry, err := r.entry(id)
	if err != nil {
		return nil, err
	}
	entry.mu.Lock()
	if entry.released || entry.cleaning {
		entry.mu.Unlock()
		return nil, service.NewError(service.NotFound, "SSH lease was not found", false, nil, nil)
	}
	if err := entry.lease.Touch(); err != nil {
		entry.mu.Unlock()
		return nil, err
	}
	entry.holds++
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	entry.mu.Unlock()
	if r.gate != nil {
		r.gate.Touch(r.now())
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Lock()
			entry.holds--
			if entry.holds == 0 && !entry.released && !entry.cleaning {
				r.scheduleExpiry(id, entry)
			}
			entry.mu.Unlock()
		})
	}, nil
}

func (r *sshLeaseRegistry) release(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, r.cleanupTimeout)
	defer cancel()
	entry, err := r.entry(id)
	if err != nil {
		return err
	}
	for {
		entry.mu.Lock()
		if entry.released {
			entry.mu.Unlock()
			return nil
		}
		if entry.cleaning {
			done := entry.cleanupDone
			entry.mu.Unlock()
			select {
			case <-ctx.Done():
				return sshCleanupFailed(ctx.Err())
			case <-done:
				continue
			}
		}
		r.beginCleanupLocked(entry)
		entry.mu.Unlock()
		return r.cleanup(ctx, id, entry)
	}

}

func (r *sshLeaseRegistry) beginCleanupLocked(entry *sshLeaseEntry) {
	entry.cleaning = true
	entry.cleanupDone = make(chan struct{})
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
}

func (r *sshLeaseRegistry) cleanup(
	ctx context.Context,
	id string,
	entry *sshLeaseEntry,
) error {
	err := sshCleanupFailed(entry.lease.Release(ctx))
	entry.mu.Lock()
	done := entry.cleanupDone
	entry.cleanupDone = nil
	entry.cleaning = false
	completed := err == nil && !entry.released
	if completed {
		entry.released = true
	} else if err != nil && !entry.released {
		r.scheduleExpiry(id, entry)
	}
	close(done)
	entry.mu.Unlock()
	if !completed {
		return err
	}
	r.mu.Lock()
	if r.leases[id] == entry {
		delete(r.leases, id)
	}
	r.mu.Unlock()
	entry.releaseReservation()
	return nil
}

func sshCleanupFailed(err error) error {
	if err == nil {
		return nil
	}
	return service.NewError(
		service.SSHCleanupFailed,
		"SSH cleanup failed",
		true,
		nil,
		err,
	)
}

func (entry *sshLeaseEntry) releaseReservation() {
	entry.reserveOnce.Do(func() {
		if entry.reservation != nil {
			entry.reservation()
		}
	})
}

func registerSSHLifecycleRoutes(mux *http.ServeMux, opts ServerOptions) {
	if opts.SSHLifecycle == nil || opts.Operations == nil {
		return
	}
	registry := opts.SSHLeases
	if registry == nil {
		registry = newSSHLeaseRegistry(opts.Gate, opts.Now, opts.SSHLeaseTimeout)
	}
	mux.HandleFunc(sshLeaseRoute, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeProblem(w, newProblem(http.StatusMethodNotAllowed, service.Descriptor{
				Code: service.InvalidRequest, Message: "unsupported SSH lease request",
			}))
			return
		}
		startSSHLease(w, r, opts, registry)
	})
	mux.HandleFunc(sshLeaseRoute+"/", func(w http.ResponseWriter, r *http.Request) {
		serveSSHLease(w, r, opts, registry)
	})
}

func startSSHLease(
	w http.ResponseWriter,
	r *http.Request,
	opts ServerOptions,
	registry *sshLeaseRegistry,
) {
	var request SSHLeaseOperationRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || requireJSONEnd(decoder) != nil {
		writeProblem(w, newProblem(http.StatusBadRequest, service.Descriptor{
			Code: service.InvalidRequest, Message: "decode SSH lease request",
		}))
		return
	}
	if request.OperationID == "" || request.Lease.Environment == nil ||
		!filepath.IsAbs(request.Lease.WorkingDirectory) {
		writeProblem(w, newProblem(http.StatusBadRequest, service.Descriptor{
			Code:    service.InvalidRequest,
			Message: "SSH lease requires an operation ID, invocation environment, and working directory",
		}))
		return
	}
	encoded, err := json.Marshal(request.Lease)
	if err != nil {
		writeProblem(w, *reportProblem(opts, r.URL.Path, err))
		return
	}
	digestValue := sha256.Sum256(encoded)
	operation, _, err := opts.Operations.Start(OperationStart{
		ID: request.OperationID, RequestDigest: hex.EncodeToString(digestValue[:]),
		Run: func(ctx context.Context, operation *Operation) (json.RawMessage, error) {
			_ = operation.Progress("Preparing SSH connection")
			leaseRequest := request.Lease
			leaseRequest.Prompt = operation.Prompt
			lease, acquireErr := opts.SSHLifecycle.Acquire(ctx, leaseRequest)
			if acquireErr != nil {
				return nil, acquireErr
			}
			if lease.Mode() != kwt.SSHLeaseModeMultiplexed {
				return nil, errors.Join(
					service.NewError(
						service.SSHRouteUnreviewable,
						"daemon SSH leases require a persistent OpenSSH master",
						false,
						nil,
						nil,
					),
					lease.Release(ctx),
				)
			}
			arguments, argumentErr := lease.Arguments(ctx)
			if argumentErr != nil {
				return nil, errors.Join(argumentErr, lease.Release(ctx))
			}
			leaseID, addErr := registry.add(lease)
			if addErr != nil {
				return nil, errors.Join(addErr, lease.Release(ctx))
			}
			result, marshalErr := json.Marshal(SSHLeaseResult{
				LeaseID: leaseID, RouteIdentity: leaseRequest.Snapshot.RouteIdentity,
				Generation: lease.Generation(), Mode: lease.Mode(), Arguments: arguments,
			})
			if marshalErr != nil {
				return nil, errors.Join(marshalErr, registry.release(ctx, leaseID))
			}
			return result, nil
		},
	})
	if err != nil {
		writeProblem(w, *reportProblem(opts, r.URL.Path, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(SSHLeaseOperation{OperationID: operation.ID()})
}

func serveSSHLease(
	w http.ResponseWriter,
	r *http.Request,
	opts ServerOptions,
	registry *sshLeaseRegistry,
) {
	suffix := strings.TrimPrefix(r.URL.Path, sshLeaseRoute+"/")
	parts := strings.Split(suffix, "/")
	var err error
	switch {
	case len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete:
		err = registry.release(r.Context(), parts[0])
	case len(parts) == 2 && parts[0] != "" && parts[1] == "touch" && r.Method == http.MethodPost:
		err = registry.touch(parts[0])
	case len(parts) == 2 && parts[0] != "" && parts[1] == "hold" && r.Method == http.MethodGet:
		var releaseHold func()
		releaseHold, err = registry.hold(parts[0])
		if err == nil {
			defer releaseHold()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
			return
		}
	default:
		writeProblem(w, newProblem(http.StatusMethodNotAllowed, service.Descriptor{
			Code: service.InvalidRequest, Message: "unsupported SSH lease request",
		}))
		return
	}
	if err != nil {
		writeProblem(w, *reportProblem(opts, r.URL.Path, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
