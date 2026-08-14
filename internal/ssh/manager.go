package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/kit/openssh"
	"go.kenn.io/kwt/service"
)

type PersistentManager interface {
	ConnectWithRunner(context.Context, string, openssh.Target, openssh.RunSSH) (openssh.Generation, error)
	ConnectionArguments(string, openssh.Generation) ([]string, error)
	TouchActivity(string, openssh.Generation) bool
	IsAlive(context.Context, string, openssh.Generation) (bool, error)
	Disconnect(context.Context, string) error
}

type RunnerFactory func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error)

type ManagerOptions struct {
	Persistent         PersistentManager
	Runner             RunnerFactory
	PrivateDirectory   string
	IdleTimeout        time.Duration
	PersistenceTimeout time.Duration
	OnEvent            func(Event)
}

type Manager struct {
	persistent         PersistentManager
	runner             RunnerFactory
	privateDirectory   string
	idleTimeout        time.Duration
	persistenceTimeout time.Duration
	onEvent            func(Event)

	mu                   sync.Mutex
	routes               map[string]*managedRoute
	operations           map[string]*identityOperation
	masterless           map[uint64]*masterlessLease
	closed               bool
	closeDone            chan struct{}
	masterlessGeneration atomic.Uint64
}

type identityOperation struct {
	gate       chan struct{}
	references int
}

type managedRoute struct {
	routeIdentity    string
	generation       openssh.Generation
	leases           int
	idle             *time.Timer
	keepAlive        *time.Timer
	keepAliveEnabled bool
	heartbeatCancel  context.CancelFunc
	heartbeatDone    chan struct{}
	forwardAgent     bool
}

func NewManager(options ManagerOptions) *Manager {
	return &Manager{
		persistent:         options.Persistent,
		runner:             options.Runner,
		privateDirectory:   options.PrivateDirectory,
		idleTimeout:        options.IdleTimeout,
		persistenceTimeout: options.PersistenceTimeout,
		onEvent:            options.OnEvent,
		routes:             make(map[string]*managedRoute),
		operations:         make(map[string]*identityOperation),
		masterless:         make(map[uint64]*masterlessLease),
	}
}

func (m *Manager) Acquire(
	ctx context.Context,
	request LeaseRequest,
	resolve func(context.Context) (RouteSnapshot, error),
) (Lease, error) {
	if m.persistent == nil || m.runner == nil {
		return nil, connectionFailed(errors.New("SSH connection manager is unavailable"))
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, connectionChanged()
	}
	current, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	if !sameRoute(request.Snapshot, current) {
		return nil, configurationChanged()
	}
	request.Snapshot = current
	if len(request.Snapshot.Targets) != 1 {
		return nil, routeUnreviewable(errors.New("SSH lifecycle requires one resolved target"))
	}
	target := request.Snapshot.Targets[0]
	runner, err := m.runner(request, target)
	if err != nil {
		return nil, connectionFailed(err)
	}
	identity := managerIdentityForLease(request.Snapshot, request.Environment)
	unlockIdentity, err := m.lockIdentityContext(ctx, identity)
	if err != nil {
		return nil, err
	}
	identityLocked := true
	releaseIdentity := func() {
		if identityLocked {
			unlockIdentity()
			identityLocked = false
		}
	}
	defer releaseIdentity()
	m.mu.Lock()
	closed = m.closed
	m.mu.Unlock()
	if closed {
		return nil, connectionChanged()
	}
	generation, err := m.persistent.ConnectWithRunner(
		ctx,
		identity,
		target.EffectiveTarget.openSSH(),
		runner,
	)
	if errors.Is(err, openssh.ErrPersistentUnsupported) {
		projectionArguments, cleanup, projectionErr := materializeProjection(
			m.privateDirectory,
			executionProjection(target),
		)
		if projectionErr != nil {
			return nil, connectionFailed(projectionErr)
		}
		arguments, argumentErr := openssh.ClientArguments("")
		if argumentErr != nil {
			_ = cleanup()
			return nil, connectionFailed(argumentErr)
		}
		arguments = append(projectionArguments, arguments...)
		current, resolveErr := resolve(ctx)
		if resolveErr != nil {
			if cleanupErr := cleanup(); cleanupErr != nil {
				return nil, cleanupFailed(cleanupErr)
			}
			return nil, resolveErr
		}
		if !sameRoute(request.Snapshot, current) {
			if cleanupErr := cleanup(); cleanupErr != nil {
				return nil, cleanupFailed(cleanupErr)
			}
			return nil, configurationChanged()
		}
		generation := m.masterlessGeneration.Add(1)
		lease := &masterlessLease{
			generation:    generation,
			arguments:     arguments,
			cleanup:       cleanup,
			manager:       m,
			routeIdentity: request.Snapshot.RouteIdentity,
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			if cleanupErr := cleanup(); cleanupErr != nil {
				return nil, cleanupFailed(cleanupErr)
			}
			return nil, connectionChanged()
		}
		m.masterless[generation] = lease
		m.mu.Unlock()
		releaseIdentity()
		m.emit(Event{
			RouteIdentity: request.Snapshot.RouteIdentity,
			Generation:    generation,
			State:         EventStateConnected,
		})
		return lease, nil
	}
	if err != nil {
		return nil, mapManagerError(err)
	}
	current, err = resolve(ctx)
	if err != nil {
		event, cleanupErr := m.disconnectUnleased(
			identity, generation, request.Snapshot.RouteIdentity, target.ForwardAgent,
		)
		if cleanupErr != nil {
			releaseIdentity()
			m.emitFailure(request.Snapshot.RouteIdentity, generation, cleanupErr)
			return nil, cleanupErr
		}
		releaseIdentity()
		m.emitOptional(event)
		return nil, err
	}
	if !sameRoute(request.Snapshot, current) {
		event, cleanupErr := m.disconnectUnleased(
			identity, generation, request.Snapshot.RouteIdentity, target.ForwardAgent,
		)
		if cleanupErr != nil {
			releaseIdentity()
			m.emitFailure(request.Snapshot.RouteIdentity, generation, cleanupErr)
			return nil, cleanupErr
		}
		releaseIdentity()
		m.emitOptional(event)
		return nil, configurationChanged()
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		event, cleanupErr := m.disconnectUnleased(
			identity, generation, request.Snapshot.RouteIdentity, target.ForwardAgent,
		)
		if cleanupErr != nil {
			releaseIdentity()
			m.emitFailure(request.Snapshot.RouteIdentity, generation, cleanupErr)
			return nil, cleanupErr
		}
		releaseIdentity()
		m.emitOptional(event)
		return nil, connectionChanged()
	}
	route := m.routes[identity]
	newGeneration := route == nil || route.generation != generation
	if newGeneration {
		if route != nil {
			m.stopPersistenceHeartbeat(route)
		}
		route = &managedRoute{
			routeIdentity: request.Snapshot.RouteIdentity,
			generation:    generation,
			forwardAgent:  target.ForwardAgent,
		}
		m.routes[identity] = route
	}
	if route.idle != nil {
		route.idle.Stop()
		route.idle = nil
	}
	route.leases++
	m.startPersistenceHeartbeat(identity, route)
	m.mu.Unlock()
	releaseIdentity()
	if newGeneration {
		m.emit(Event{
			RouteIdentity: request.Snapshot.RouteIdentity,
			Generation:    uint64(generation),
			State:         EventStateConnected,
		})
	}
	return &managedLease{
		manager: m, identity: identity, generation: generation,
	}, nil
}

func (m *Manager) disconnectUnleased(
	identity string,
	generation openssh.Generation,
	routeIdentity string,
	forwardAgent bool,
) (*Event, *service.Error) {
	m.mu.Lock()
	route := m.routes[identity]
	if route != nil && route.generation != generation {
		m.mu.Unlock()
		return nil, connectionChanged()
	}
	if route != nil && route.leases > 0 {
		m.mu.Unlock()
		return nil, nil
	}
	if route == nil {
		route = &managedRoute{
			routeIdentity: routeIdentity,
			generation:    generation,
			forwardAgent:  forwardAgent,
		}
		m.routes[identity] = route
	}
	if route.idle != nil {
		route.idle.Stop()
		route.idle = nil
	}
	heartbeatDone := m.stopPersistenceHeartbeat(route)
	m.mu.Unlock()
	_ = waitPersistenceHeartbeat(context.Background(), heartbeatDone)
	if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
		return nil, cleanupFailed(err)
	}
	m.mu.Lock()
	if m.routes[identity] == route {
		delete(m.routes, identity)
	}
	m.mu.Unlock()
	return &Event{
		RouteIdentity: route.routeIdentity,
		Generation:    uint64(generation),
		State:         EventStateDisconnected,
	}, nil
}

func managerIdentity(snapshot RouteSnapshot) string {
	return snapshot.ProjectionPolicy + ":" + snapshot.RouteIdentity
}

func managerIdentityForLease(snapshot RouteSnapshot, environment []string) string {
	identity := managerIdentity(snapshot)
	if len(snapshot.Targets) != 1 || !snapshot.Targets[0].ForwardAgent {
		return identity
	}
	digest := sha256.New()
	for _, entry := range environment {
		_, _ = digest.Write([]byte(entry))
		_, _ = digest.Write([]byte{0})
	}
	return identity + ":agent:" + hex.EncodeToString(digest.Sum(nil))
}

func sameRoute(expected, current RouteSnapshot) bool {
	return expected.RouteIdentity == current.RouteIdentity &&
		expected.ProjectionPolicy == current.ProjectionPolicy
}

type managedLease struct {
	manager    *Manager
	identity   string
	generation openssh.Generation
	released   atomic.Bool
}

func (l *managedLease) Mode() LeaseMode { return LeaseModeMultiplexed }

func (l *managedLease) Generation() uint64 { return uint64(l.generation) }

func (l *managedLease) Arguments(ctx context.Context) ([]string, error) {
	if l.released.Load() {
		return nil, connectionChanged()
	}
	arguments, err := l.manager.persistent.ConnectionArguments(l.identity, l.generation)
	if err != nil {
		return nil, mapManagerError(err)
	}
	return arguments, nil
}

func (l *managedLease) Touch() error {
	if l.released.Load() || !l.manager.persistent.TouchActivity(l.identity, l.generation) {
		return connectionChanged()
	}
	return nil
}

func (l *managedLease) Release() error {
	if !l.released.CompareAndSwap(false, true) {
		return nil
	}
	return l.manager.release(l.identity, l.generation)
}

// startPersistenceHeartbeat keeps OpenSSH's crash-fallback ControlPersist
// timer from expiring while kwt still owns the route. Callers hold m.mu.
func (m *Manager) startPersistenceHeartbeat(identity string, route *managedRoute) {
	if m.persistenceTimeout <= 0 || m.closed {
		return
	}
	route.keepAliveEnabled = true
	m.schedulePersistenceHeartbeat(identity, route)
}

// schedulePersistenceHeartbeat schedules one generation-bound mux check.
// Callers hold m.mu.
func (m *Manager) schedulePersistenceHeartbeat(identity string, route *managedRoute) {
	if !route.keepAliveEnabled || route.keepAlive != nil {
		return
	}
	interval := m.persistenceTimeout / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	route.keepAlive = time.AfterFunc(interval, func() {
		m.refreshPersistence(identity, route, interval)
	})
}

func (m *Manager) refreshPersistence(
	identity string,
	route *managedRoute,
	timeout time.Duration,
) {
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	m.mu.Lock()
	if m.closed || m.routes[identity] != route || !route.keepAliveEnabled {
		m.mu.Unlock()
		return
	}
	route.keepAlive = nil
	generation := route.generation
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	done := make(chan struct{})
	route.heartbeatCancel = cancel
	route.heartbeatDone = done
	m.mu.Unlock()

	_, _ = m.persistent.IsAlive(ctx, identity, generation)
	cancel()

	m.mu.Lock()
	if route.heartbeatDone == done {
		route.heartbeatCancel = nil
		route.heartbeatDone = nil
		if !m.closed && m.routes[identity] == route && route.keepAliveEnabled {
			m.schedulePersistenceHeartbeat(identity, route)
		}
	}
	m.mu.Unlock()
	close(done)
}

// stopPersistenceHeartbeat prevents an in-flight mux check from rescheduling
// itself once route teardown begins. Callers hold m.mu.
func (m *Manager) stopPersistenceHeartbeat(route *managedRoute) <-chan struct{} {
	route.keepAliveEnabled = false
	if route.keepAlive != nil {
		route.keepAlive.Stop()
		route.keepAlive = nil
	}
	if route.heartbeatCancel != nil {
		route.heartbeatCancel()
	}
	return route.heartbeatDone
}

func waitPersistenceHeartbeat(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) release(identity string, generation openssh.Generation) error {
	unlockIdentity := m.lockIdentity(identity)

	m.mu.Lock()
	route := m.routes[identity]
	if route == nil || route.generation != generation {
		closed := m.closed
		m.mu.Unlock()
		unlockIdentity()
		if closed {
			return nil
		}
		return connectionChanged()
	}
	route.leases--
	if route.leases > 0 {
		m.mu.Unlock()
		unlockIdentity()
		return nil
	}
	if m.idleTimeout > 0 && !route.forwardAgent {
		route.idle = time.AfterFunc(m.idleTimeout, func() {
			m.disconnectIdle(identity, generation)
		})
		m.mu.Unlock()
		unlockIdentity()
		return nil
	}
	heartbeatDone := m.stopPersistenceHeartbeat(route)
	routeIdentity := route.routeIdentity
	m.mu.Unlock()
	_ = waitPersistenceHeartbeat(context.Background(), heartbeatDone)
	if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
		failure := cleanupFailed(err)
		unlockIdentity()
		m.emitFailure(routeIdentity, generation, failure)
		return failure
	}
	m.mu.Lock()
	if m.routes[identity] == route {
		delete(m.routes, identity)
	}
	m.mu.Unlock()
	unlockIdentity()
	m.emit(Event{
		RouteIdentity: routeIdentity,
		Generation:    uint64(generation),
		State:         EventStateDisconnected,
	})
	return nil
}

func (m *Manager) disconnectIdle(identity string, generation openssh.Generation) {
	unlockIdentity := m.lockIdentity(identity)

	m.mu.Lock()
	route := m.routes[identity]
	if route == nil || route.generation != generation || route.leases != 0 {
		m.mu.Unlock()
		unlockIdentity()
		return
	}
	route.idle = nil
	heartbeatDone := m.stopPersistenceHeartbeat(route)
	routeIdentity := route.routeIdentity
	m.mu.Unlock()
	_ = waitPersistenceHeartbeat(context.Background(), heartbeatDone)
	if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
		unlockIdentity()
		m.emitFailure(routeIdentity, generation, cleanupFailed(err))
		return
	}
	m.mu.Lock()
	if m.routes[identity] == route {
		delete(m.routes, identity)
	}
	m.mu.Unlock()
	unlockIdentity()
	m.emit(Event{
		RouteIdentity: routeIdentity,
		Generation:    uint64(generation),
		State:         EventStateDisconnected,
	})
}

func (m *Manager) Close(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		m.closed = true
		if m.closeDone != nil {
			done := m.closeDone
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				continue
			}
		}
		if len(m.routes) == 0 && len(m.operations) == 0 && len(m.masterless) == 0 {
			m.mu.Unlock()
			return nil
		}
		done := make(chan struct{})
		m.closeDone = done
		routeIdentities := make([]string, 0, len(m.routes))
		for identity, route := range m.routes {
			routeIdentities = append(routeIdentities, identity)
			if route.idle != nil {
				route.idle.Stop()
				route.idle = nil
			}
			m.stopPersistenceHeartbeat(route)
		}
		operationIdentities := make([]string, 0, len(m.operations))
		for identity := range m.operations {
			if _, established := m.routes[identity]; !established {
				operationIdentities = append(operationIdentities, identity)
			}
		}
		masterless := make([]*masterlessLease, 0, len(m.masterless))
		for _, lease := range m.masterless {
			masterless = append(masterless, lease)
		}
		m.mu.Unlock()

		events, closeErr := m.closeAttempt(
			ctx,
			routeIdentities,
			operationIdentities,
			masterless,
		)
		m.mu.Lock()
		m.closeDone = nil
		close(done)
		m.mu.Unlock()
		for _, event := range events {
			m.emit(event)
		}
		return closeErr
	}
}

func (m *Manager) closeAttempt(
	ctx context.Context,
	routeIdentities []string,
	operationIdentities []string,
	masterless []*masterlessLease,
) ([]Event, error) {
	var closeErr error
	events := make([]Event, 0, len(routeIdentities)+len(masterless))

	for _, lease := range masterless {
		lease.released.Store(true)
		cleaned, err := lease.cleanupTracked()
		if err != nil {
			failure := cleanupFailed(err)
			closeErr = errors.Join(closeErr, failure)
			descriptor := failure.Descriptor
			events = append(events, Event{
				RouteIdentity: lease.routeIdentity,
				Generation:    lease.generation,
				State:         EventStateError,
				Failure:       &descriptor,
			})
			continue
		}
		if !cleaned {
			continue
		}
		events = append(events, Event{
			RouteIdentity: lease.routeIdentity,
			Generation:    lease.generation,
			State:         EventStateDisconnected,
		})
	}

	sort.Strings(routeIdentities)
	for _, identity := range routeIdentities {
		unlockIdentity, err := m.lockIdentityContext(ctx, identity)
		if err != nil {
			closeErr = errors.Join(closeErr, err)
			break
		}
		m.mu.Lock()
		route := m.routes[identity]
		var heartbeatDone <-chan struct{}
		if route != nil {
			if route.idle != nil {
				route.idle.Stop()
			}
			heartbeatDone = m.stopPersistenceHeartbeat(route)
		}
		m.mu.Unlock()
		if route == nil {
			unlockIdentity()
			continue
		}
		if err = waitPersistenceHeartbeat(ctx, heartbeatDone); err != nil {
			unlockIdentity()
			closeErr = errors.Join(closeErr, err)
			break
		}
		err = m.persistent.Disconnect(ctx, identity)
		if err != nil {
			unlockIdentity()
			failure := cleanupFailed(err)
			closeErr = errors.Join(closeErr, failure)
			descriptor := failure.Descriptor
			events = append(events, Event{
				RouteIdentity: route.routeIdentity,
				Generation:    uint64(route.generation),
				State:         EventStateError,
				Failure:       &descriptor,
			})
			continue
		}
		m.mu.Lock()
		if m.routes[identity] == route {
			delete(m.routes, identity)
		}
		m.mu.Unlock()
		unlockIdentity()
		events = append(events, Event{
			RouteIdentity: route.routeIdentity,
			Generation:    uint64(route.generation),
			State:         EventStateDisconnected,
		})
	}

	if ctx.Err() == nil {
		sort.Strings(operationIdentities)
		for _, identity := range operationIdentities {
			unlockIdentity, err := m.lockIdentityContext(ctx, identity)
			if err != nil {
				closeErr = errors.Join(closeErr, err)
				break
			}
			unlockIdentity()
		}
	}
	return events, closeErr
}

func (m *Manager) lockIdentity(identity string) func() {
	unlock, _ := m.lockIdentityContext(context.Background(), identity)
	return unlock
}

func (m *Manager) lockIdentityContext(
	ctx context.Context,
	identity string,
) (func(), error) {
	m.mu.Lock()
	operation := m.operations[identity]
	if operation == nil {
		operation = &identityOperation{gate: make(chan struct{}, 1)}
		operation.gate <- struct{}{}
		m.operations[identity] = operation
	}
	operation.references++
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		m.mu.Lock()
		operation.references--
		if operation.references == 0 {
			delete(m.operations, identity)
		}
		m.mu.Unlock()
		return nil, ctx.Err()
	case <-operation.gate:
		if err := ctx.Err(); err != nil {
			operation.gate <- struct{}{}
			m.mu.Lock()
			operation.references--
			if operation.references == 0 {
				delete(m.operations, identity)
			}
			m.mu.Unlock()
			return nil, err
		}
	}
	return func() {
		operation.gate <- struct{}{}
		m.mu.Lock()
		operation.references--
		if operation.references == 0 {
			delete(m.operations, identity)
		}
		m.mu.Unlock()
	}, nil
}

type masterlessLease struct {
	generation    uint64
	arguments     []string
	cleanup       func() error
	manager       *Manager
	routeIdentity string
	released      atomic.Bool
	cleanupMu     sync.Mutex
}

func (l *masterlessLease) Mode() LeaseMode { return LeaseModeMasterless }

func (l *masterlessLease) Generation() uint64 { return l.generation }

func (l *masterlessLease) Arguments(context.Context) ([]string, error) {
	if l.released.Load() {
		return nil, connectionChanged()
	}
	return append([]string(nil), l.arguments...), nil
}

func (l *masterlessLease) Touch() error {
	if l.released.Load() {
		return connectionChanged()
	}
	return nil
}

func (l *masterlessLease) Release() error {
	l.released.Store(true)
	cleaned, err := l.cleanupTracked()
	if err != nil {
		failure := cleanupFailed(err)
		l.manager.emitFailure(l.routeIdentity, openssh.Generation(l.generation), failure)
		return failure
	}
	if !cleaned {
		return nil
	}
	l.manager.emit(Event{
		RouteIdentity: l.routeIdentity,
		Generation:    l.generation,
		State:         EventStateDisconnected,
	})
	return nil
}

func (l *masterlessLease) cleanupTracked() (bool, error) {
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()

	l.manager.mu.Lock()
	tracked := l.manager.masterless[l.generation] == l
	l.manager.mu.Unlock()
	if !tracked {
		return false, nil
	}
	if l.cleanup != nil {
		if err := l.cleanup(); err != nil {
			return true, err
		}
	}
	l.manager.mu.Lock()
	if l.manager.masterless[l.generation] == l {
		delete(l.manager.masterless, l.generation)
	}
	l.manager.mu.Unlock()
	return true, nil
}

func (m *Manager) emitFailure(
	routeIdentity string,
	generation openssh.Generation,
	failure *service.Error,
) {
	descriptor := failure.Descriptor
	m.emit(Event{
		RouteIdentity: routeIdentity,
		Generation:    uint64(generation),
		State:         EventStateError,
		Failure:       &descriptor,
	})
}

func (m *Manager) emit(event Event) {
	if m.onEvent != nil {
		m.onEvent(event)
	}
}

func (m *Manager) emitOptional(event *Event) {
	if event != nil {
		m.emit(*event)
	}
}

func mapManagerError(err error) error {
	var typed *service.Error
	switch {
	case errors.As(err, &typed):
		return typed
	case errors.Is(err, openssh.ErrConnectionChanged):
		return connectionChanged()
	case errors.Is(err, openssh.ErrControlPathOccupied):
		return service.NewError(
			service.SSHControlPathOccupied,
			"SSH control path is occupied",
			false,
			nil,
			err,
		)
	default:
		return connectionFailed(err)
	}
}

func connectionFailed(err error) *service.Error {
	return service.NewError(
		service.SSHConnectionFailed,
		"SSH connection failed",
		true,
		nil,
		err,
	)
}

func cleanupFailed(err error) *service.Error {
	return service.NewError(
		service.SSHCleanupFailed,
		"SSH cleanup failed",
		true,
		nil,
		err,
	)
}
