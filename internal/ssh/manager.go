package ssh

import (
	"context"
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
	Disconnect(context.Context, string) error
}

type RunnerFactory func(LeaseRequest, ResolvedTarget) (openssh.RunSSH, error)

type ManagerOptions struct {
	Persistent       PersistentManager
	Runner           RunnerFactory
	PrivateDirectory string
	IdleTimeout      time.Duration
	OnEvent          func(Event)
}

type Manager struct {
	persistent       PersistentManager
	runner           RunnerFactory
	privateDirectory string
	idleTimeout      time.Duration
	onEvent          func(Event)

	mu                   sync.Mutex
	routes               map[string]*managedRoute
	operations           map[string]*identityOperation
	masterless           map[uint64]*masterlessLease
	closed               bool
	closeDone            chan struct{}
	closeErr             error
	masterlessGeneration atomic.Uint64
}

type identityOperation struct {
	mu         sync.Mutex
	references int
}

type managedRoute struct {
	routeIdentity string
	generation    openssh.Generation
	leases        int
	idle          *time.Timer
}

func NewManager(options ManagerOptions) *Manager {
	return &Manager{
		persistent:       options.Persistent,
		runner:           options.Runner,
		privateDirectory: options.PrivateDirectory,
		idleTimeout:      options.IdleTimeout,
		onEvent:          options.OnEvent,
		routes:           make(map[string]*managedRoute),
		operations:       make(map[string]*identityOperation),
		masterless:       make(map[uint64]*masterlessLease),
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
	identity := managerIdentity(request.Snapshot)
	unlockIdentity := m.lockIdentity(identity)
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
		if cleanupErr := m.disconnectUnleased(
			identity, generation,
		); cleanupErr != nil {
			releaseIdentity()
			m.emitFailure(request.Snapshot.RouteIdentity, generation, cleanupErr)
			return nil, cleanupErr
		}
		return nil, err
	}
	if !sameRoute(request.Snapshot, current) {
		if cleanupErr := m.disconnectUnleased(
			identity, generation,
		); cleanupErr != nil {
			releaseIdentity()
			m.emitFailure(request.Snapshot.RouteIdentity, generation, cleanupErr)
			return nil, cleanupErr
		}
		return nil, configurationChanged()
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		if cleanupErr := m.disconnectUnleased(
			identity, generation,
		); cleanupErr != nil {
			releaseIdentity()
			m.emitFailure(request.Snapshot.RouteIdentity, generation, cleanupErr)
			return nil, cleanupErr
		}
		return nil, connectionChanged()
	}
	route := m.routes[identity]
	newGeneration := route == nil || route.generation != generation
	if newGeneration {
		route = &managedRoute{
			routeIdentity: request.Snapshot.RouteIdentity,
			generation:    generation,
		}
		m.routes[identity] = route
	}
	if route.idle != nil {
		route.idle.Stop()
		route.idle = nil
	}
	route.leases++
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
) *service.Error {
	m.mu.Lock()
	route := m.routes[identity]
	active := route != nil && route.generation == generation && route.leases > 0
	m.mu.Unlock()
	if !active {
		if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
			return cleanupFailed(err)
		}
	}
	return nil
}

func managerIdentity(snapshot RouteSnapshot) string {
	return snapshot.ProjectionPolicy + ":" + snapshot.RouteIdentity
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
	if m.idleTimeout > 0 {
		route.idle = time.AfterFunc(m.idleTimeout, func() {
			m.disconnectIdle(identity, generation)
		})
		m.mu.Unlock()
		unlockIdentity()
		return nil
	}
	delete(m.routes, identity)
	routeIdentity := route.routeIdentity
	m.mu.Unlock()
	if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
		failure := cleanupFailed(err)
		unlockIdentity()
		m.emitFailure(routeIdentity, generation, failure)
		return failure
	}
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
	delete(m.routes, identity)
	routeIdentity := route.routeIdentity
	m.mu.Unlock()
	if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
		unlockIdentity()
		m.emitFailure(routeIdentity, generation, cleanupFailed(err))
		return
	}
	unlockIdentity()
	m.emit(Event{
		RouteIdentity: routeIdentity,
		Generation:    uint64(generation),
		State:         EventStateDisconnected,
	})
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		err := m.closeErr
		m.mu.Unlock()
		if done != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
			}
			m.mu.Lock()
			err = m.closeErr
			m.mu.Unlock()
		}
		return err
	}
	m.closed = true
	done := make(chan struct{})
	m.closeDone = done
	identities := make(map[string]struct{}, len(m.routes)+len(m.operations))
	for identity, route := range m.routes {
		identities[identity] = struct{}{}
		if route.idle != nil {
			route.idle.Stop()
			route.idle = nil
		}
	}
	for identity := range m.operations {
		identities[identity] = struct{}{}
	}
	masterless := make([]*masterlessLease, 0, len(m.masterless))
	for _, lease := range m.masterless {
		masterless = append(masterless, lease)
	}
	m.mu.Unlock()

	var closeErr error
	events := make([]Event, 0, len(identities)+len(masterless))
	for _, lease := range masterless {
		if !lease.released.CompareAndSwap(false, true) {
			continue
		}
		m.mu.Lock()
		delete(m.masterless, lease.generation)
		m.mu.Unlock()
		if err := lease.cleanup(); err != nil {
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
		events = append(events, Event{
			RouteIdentity: lease.routeIdentity,
			Generation:    lease.generation,
			State:         EventStateDisconnected,
		})
	}

	ordered := make([]string, 0, len(identities))
	for identity := range identities {
		ordered = append(ordered, identity)
	}
	sort.Strings(ordered)
	for _, identity := range ordered {
		unlockIdentity := m.lockIdentity(identity)
		m.mu.Lock()
		route := m.routes[identity]
		if route != nil {
			if route.idle != nil {
				route.idle.Stop()
			}
			delete(m.routes, identity)
		}
		m.mu.Unlock()
		if route == nil {
			unlockIdentity()
			continue
		}
		err := m.persistent.Disconnect(ctx, identity)
		unlockIdentity()
		if err != nil {
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
		events = append(events, Event{
			RouteIdentity: route.routeIdentity,
			Generation:    uint64(route.generation),
			State:         EventStateDisconnected,
		})
	}

	m.mu.Lock()
	m.closeErr = closeErr
	m.closeDone = nil
	close(done)
	m.mu.Unlock()
	for _, event := range events {
		m.emit(event)
	}
	return closeErr
}

func (m *Manager) lockIdentity(identity string) func() {
	m.mu.Lock()
	operation := m.operations[identity]
	if operation == nil {
		operation = &identityOperation{}
		m.operations[identity] = operation
	}
	operation.references++
	m.mu.Unlock()

	operation.mu.Lock()
	return func() {
		operation.mu.Unlock()
		m.mu.Lock()
		operation.references--
		if operation.references == 0 {
			delete(m.operations, identity)
		}
		m.mu.Unlock()
	}
}

type masterlessLease struct {
	generation    uint64
	arguments     []string
	cleanup       func() error
	manager       *Manager
	routeIdentity string
	released      atomic.Bool
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
	if !l.released.CompareAndSwap(false, true) {
		return nil
	}
	l.manager.mu.Lock()
	delete(l.manager.masterless, l.generation)
	l.manager.mu.Unlock()
	if l.cleanup == nil {
		l.manager.emit(Event{
			RouteIdentity: l.routeIdentity,
			Generation:    l.generation,
			State:         EventStateDisconnected,
		})
		return nil
	}
	if err := l.cleanup(); err != nil {
		failure := cleanupFailed(err)
		l.manager.emitFailure(l.routeIdentity, openssh.Generation(l.generation), failure)
		return failure
	}
	l.manager.emit(Event{
		RouteIdentity: l.routeIdentity,
		Generation:    l.generation,
		State:         EventStateDisconnected,
	})
	return nil
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

func mapManagerError(err error) error {
	switch {
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
