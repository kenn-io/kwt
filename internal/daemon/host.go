package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 2 * time.Second
	httpIdleTimeout       = 30 * time.Second
	httpMaxHeaderBytes    = 16 << 10
	forcedDrainCleanup    = 5 * time.Second
)

type ServeOptions struct {
	Home              string
	Build             Build
	Config            models.DaemonConfig
	Foreground        bool
	Now               func() time.Time
	IdleCheckInterval time.Duration
	Stdout            io.Writer
	Stderr            io.Writer
	Inventory         kwt.Inventory
	Remover           kwt.Remover
	ProjectRemover    kwt.ProjectRemover
	SSHResolver       SSHResolver
	SSHLifecycle      SSHLifecycle
}

type hostStatus struct {
	mu        sync.RWMutex
	base      Status
	gate      *Gate
	lastError *Failure
}

func (s *hostStatus) Status(now time.Time) Status {
	s.mu.RLock()
	status := s.base
	status.LastError = s.lastError
	s.mu.RUnlock()
	snapshot := s.gate.Snapshot()
	status.ActiveWork = snapshot.ActiveWork
	status.ActiveLeases = snapshot.ActiveLeases
	status.UptimeSeconds = int64(now.Sub(status.StartedAt).Seconds())
	if snapshot.Draining {
		status.State = StateDraining
		deadline := snapshot.DrainDeadline
		status.DrainDeadline = &deadline
	}
	return status
}

func (s *hostStatus) setState(state State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.base.State = state
}

func (s *hostStatus) fail(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.base.State = StateFailed
	s.lastError = &Failure{At: now, Message: boundedError(err)}
}

func Serve(ctx context.Context, opts ServeOptions) error {
	if !filepath.IsAbs(opts.Home) {
		return fmt.Errorf("daemon home %q must be absolute", opts.Home)
	}
	if opts.Config.ReplacementGrace <= 0 {
		return errors.New("daemon replacement grace must be positive")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IdleCheckInterval <= 0 {
		opts.IdleCheckInterval = time.Second
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}

	store := RuntimeStore(opts.Home)
	ownerCtx, cancelOwner := context.WithTimeout(ctx, 100*time.Millisecond)
	releaseOwner, err := store.AcquireOwnerLock(ownerCtx)
	cancelOwner()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return service.NewError(
				service.Conflict,
				"another kwt daemon owner is active",
				false,
				nil,
				err,
			)
		}
		return err
	}
	defer releaseOwner()

	return runHost(ctx, opts, store)
}

func runHost(
	ctx context.Context,
	opts ServeOptions,
	store kitdaemon.RuntimeStore,
) (returnErr error) {
	ep, err := kitdaemon.ParseEndpoint(
		"127.0.0.1:0",
		kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback},
	)
	if err != nil {
		return err
	}
	listener, err := kitdaemon.Listen(ctx, ep, kitdaemon.WithRuntimeStore(store))
	if err != nil {
		return err
	}
	ep.Address = listener.Addr().String()

	record, token, err := NewRuntimeRecord(opts.Home, opts.Build, ep)
	if err != nil {
		_ = listener.Close()
		return err
	}
	proof, err := kitdaemon.NewProof([]byte(token))
	if err != nil {
		_ = listener.Close()
		return err
	}
	ping, err := proof.NewPingHandler(record)
	if err != nil {
		_ = listener.Close()
		return err
	}

	logWriter, logger, err := hostLogger(opts)
	if err != nil {
		_ = listener.Close()
		return err
	}
	if logWriter != nil {
		defer func() { returnErr = errors.Join(returnErr, logWriter.Close()) }()
	}

	startedAt := opts.Now()
	gate := NewGate(startedAt)
	operationContext, cancelOperations := newHostOperationContext(ctx)
	defer cancelOperations()
	operations := NewOperationHub(operationContext, OperationHubOptions{
		Now: opts.Now,
		Reserve: func() (func(), error) {
			return gate.Reserve(ReservationWork, opts.Now())
		},
	})
	inventory := opts.Inventory
	remover := opts.Remover
	projectRemover := opts.ProjectRemover
	sshResolver := opts.SSHResolver
	sshLifecycle := opts.SSHLifecycle
	var cacheDiagnostic *kwt.Diagnostic
	if inventory == nil {
		cache, diagnostic, cacheErr := kwt.NewFileCache(opts.Home)
		var serviceCache kwt.Cache
		if cacheErr != nil {
			cacheDiagnostic = &kwt.Diagnostic{
				At: opts.Now(), Message: boundedError(cacheErr),
			}
		} else {
			cacheDiagnostic = diagnostic
			serviceCache = cache
		}
		inventory = kwt.NewInventoryService(kwt.InventoryServiceOptions{
			Source:  kwt.NewSource(kwt.SourceOptions{Home: opts.Home}),
			Cache:   serviceCache,
			Now:     opts.Now,
			Context: ctx,
		})
	}
	if remover == nil {
		remover = kwt.NewRemovalService(kwt.RemovalServiceOptions{
			Home: opts.Home,
		})
	}
	if projectRemover == nil {
		projectRemover = kwt.NewProjectRemovalService(kwt.ProjectRemovalServiceOptions{
			Home: opts.Home,
		})
	}
	if sshResolver == nil {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			return executableErr
		}
		serviceOwner := kwt.NewSSHService(kwt.SSHServiceOptions{
			Home:              opts.Home,
			AskpassExecutable: executable,
			Now:               opts.Now,
		})
		sshResolver = serviceOwner
		if sshLifecycle == nil {
			sshLifecycle = serviceOwner
		}
	} else if sshLifecycle == nil {
		sshLifecycle, _ = sshResolver.(SSHLifecycle)
	}
	sshLeases := newSSHLeaseRegistry(gate, opts.Now, 0)
	status := &hostStatus{
		base: Status{
			Service:       ServiceName,
			State:         StateStarting,
			Home:          opts.Home,
			Endpoint:      ep.Address,
			PID:           record.PID,
			Version:       opts.Build.Version,
			Revision:      opts.Build.Revision,
			RevisionTime:  opts.Build.RevisionTime,
			SchemaMajor:   APISchemaMajor,
			SchemaVersion: APISchemaVersion,
			Capabilities: []string{
				CapabilityShutdown,
				CapabilityStatus,
				CapabilityOperationStream,
				CapabilityProjectRemoval,
				CapabilitySSHLeaseHold,
				CapabilitySSHLifecycle,
				CapabilitySSHResolve,
				CapabilityInventory,
				CapabilityRemoval,
				CapabilityGuardedRemoval,
			},
			StartedAt: startedAt,
		},
		gate: gate,
	}
	if cacheDiagnostic != nil {
		status.lastError = &Failure{At: cacheDiagnostic.At, Message: cacheDiagnostic.Message}
	}
	shutdownRequested := make(chan string, 1)
	shutdown := func(_ context.Context, request ShutdownRequest) (Status, error) {
		deadline := opts.Now().Add(opts.Config.ReplacementGrace)
		gate.BeginDrain(deadline)
		operations.BeginDrain(deadline)
		select {
		case shutdownRequested <- request.Reason:
		default:
		}
		return status.Status(opts.Now()), nil
	}
	diagnosticSecrets := processDiagnosticSecrets(
		token,
		configuredFleetTokenEnvironment(opts.Home),
	)
	handler := NewServer(ServerOptions{
		Token:          token,
		ExpectedHost:   ep.Address,
		Status:         status,
		Shutdown:       shutdown,
		Ping:           ping,
		Touch:          gate.Touch,
		Now:            opts.Now,
		Inventory:      inventory,
		Remover:        remover,
		ProjectRemover: projectRemover,
		Operations:     operations,
		SSHResolver:    sshResolver,
		SSHLifecycle:   sshLifecycle,
		SSHLeases:      sshLeases,
		Gate:           gate,
		ReportError: func(
			route string,
			failure *service.Error,
			expansion kwt.ExpansionContext,
		) {
			secrets := append([]string(nil), diagnosticSecrets...)
			secrets = append(
				secrets,
				invocationDiagnosticSecrets(opts.Home, expansion)...,
			)
			logServiceFailure(logger, route, failure, secrets)
		},
	})
	httpServer := newHTTPServer(handler)
	serverDone := make(chan error, 1)
	go func() {
		serveErr := httpServer.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		serverDone <- serveErr
	}()

	runtimePath, err := store.Write(record)
	if err != nil {
		shutdownErr := shutdownHTTPServer(
			httpServer,
			time.Now().Add(opts.Config.ReplacementGrace),
			DrainReleased,
		)
		_ = listener.Close()
		return errors.Join(err, shutdownErr)
	}
	status.setState(StateReady)
	logLifecycle(logger, "ready", status.Status(opts.Now()), nil)

	var idleTicker *time.Ticker
	var idle <-chan time.Time
	if !opts.Foreground {
		idleTicker = time.NewTicker(opts.IdleCheckInterval)
		idle = idleTicker.C
		defer idleTicker.Stop()
	}

	var runErr error
	selecting := true
	for selecting {
		select {
		case <-ctx.Done():
			selecting = false
		case <-shutdownRequested:
			selecting = false
		case serveErr := <-serverDone:
			runErr = serveErr
			if serveErr != nil {
				status.fail(opts.Now(), serveErr)
			}
			selecting = false
		case now := <-idle:
			if gate.ShouldStopForIdle(now, opts.Config.IdleTimeout) {
				selecting = false
			}
		}
	}

	drain := gate.BeginDrain(opts.Now().Add(opts.Config.ReplacementGrace))
	operations.BeginDrain(drain.DrainDeadline)
	logLifecycle(logger, "draining", status.Status(opts.Now()), runErr)
	drainResult := gate.WaitForDrain(context.Background(), opts.Now())
	if drainResult != DrainReleased {
		operations.CancelActiveForDrain()
		cancelOperations()
	}
	shutdownErr := shutdownHTTPServer(httpServer, drain.DrainDeadline, drainResult)
	sshCleanupCtx, cancelSSHCleanup := context.WithTimeout(
		context.Background(), forcedDrainCleanup,
	)
	sshCleanupErr := errors.Join(
		sshLeases.close(sshCleanupCtx),
		closeSSHOwners(sshCleanupCtx, sshResolver, sshLifecycle),
	)
	cancelSSHCleanup()
	var cleanupErr error
	if drainResult != DrainReleased {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), forcedDrainCleanup)
		if !gate.WaitForRelease(cleanupCtx) {
			cleanupErr = errors.New("daemon cleanup deadline expired")
		}
		cancelCleanup()
	}
	_ = listener.Close()
	removeErr := removeOwnedRuntime(runtimePath, store, record.PID)
	stopErr := errors.Join(runErr, shutdownErr, sshCleanupErr, cleanupErr, removeErr)
	logLifecycle(logger, "stopped", status.Status(opts.Now()), stopErr)
	return stopErr
}

type sshOwner interface {
	Close(context.Context) error
}

func closeSSHOwners(
	ctx context.Context,
	resolver SSHResolver,
	lifecycle SSHLifecycle,
) error {
	resolverOwner, resolverOwned := resolver.(sshOwner)
	lifecycleOwner, lifecycleOwned := lifecycle.(sshOwner)
	if resolverOwned && lifecycleOwned && sameSSHOwner(resolverOwner, lifecycleOwner) {
		return resolverOwner.Close(ctx)
	}
	var resolverErr, lifecycleErr error
	if resolverOwned {
		resolverErr = resolverOwner.Close(ctx)
	}
	if lifecycleOwned {
		lifecycleErr = lifecycleOwner.Close(ctx)
	}
	return errors.Join(resolverErr, lifecycleErr)
}

func sameSSHOwner(left, right sshOwner) bool {
	leftType := reflect.TypeOf(left)
	return leftType == reflect.TypeOf(right) && leftType.Comparable() && left == right
}

func newHostOperationContext(hostContext context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(context.WithoutCancel(hostContext))
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
	}
}

func hostLogger(opts ServeOptions) (*rotatingLog, *slog.Logger, error) {
	if opts.Foreground {
		return nil, slog.New(slog.NewJSONHandler(opts.Stderr, nil)), nil
	}
	writer, err := openBackgroundLog(opts.Home)
	if err != nil {
		return nil, nil, err
	}
	return writer, slog.New(slog.NewJSONHandler(writer, nil)), nil
}

func logLifecycle(logger *slog.Logger, event string, status Status, err error) {
	attributes := []any{
		"event", event,
		"pid", status.PID,
		"version", status.Version,
		"state", status.State,
	}
	if err != nil {
		attributes = append(attributes, "error", boundedError(err))
	}
	logger.Info("daemon lifecycle", attributes...)
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	const maximum = 1024
	message := err.Error()
	if len(message) <= maximum {
		return message
	}
	return message[:maximum]
}

func shutdownHTTPServer(
	server *http.Server,
	deadline time.Time,
	drainResult DrainResult,
) error {
	if drainResult != DrainReleased {
		return closeHTTPServer(server)
	}
	shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Join(err, closeHTTPServer(server))
	}
	return nil
}

func closeHTTPServer(server *http.Server) error {
	if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func removeOwnedRuntime(
	runtimePath string,
	store kitdaemon.RuntimeStore,
	pid int,
) error {
	expected, err := store.Path(pid)
	if err != nil {
		return err
	}
	if runtimePath != expected {
		return fmt.Errorf("runtime record path %q does not match %q", runtimePath, expected)
	}
	if err := os.Remove(expected); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon runtime record: %w", err)
	}
	return nil
}
