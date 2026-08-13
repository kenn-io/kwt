package ssh

import (
	"context"
	"errors"
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

	mu         sync.Mutex
	routes     map[string]*managedRoute
	operations map[string]*identityOperation
	masterless atomic.Uint64
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
	defer unlockIdentity()
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
		generation := m.masterless.Add(1)
		lease := &masterlessLease{
			generation:    generation,
			arguments:     arguments,
			cleanup:       cleanup,
			manager:       m,
			routeIdentity: request.Snapshot.RouteIdentity,
		}
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
			identity,
			request.Snapshot.RouteIdentity,
			generation,
		); cleanupErr != nil {
			return nil, cleanupErr
		}
		return nil, err
	}
	if !sameRoute(request.Snapshot, current) {
		if cleanupErr := m.disconnectUnleased(
			identity,
			request.Snapshot.RouteIdentity,
			generation,
		); cleanupErr != nil {
			return nil, cleanupErr
		}
		return nil, configurationChanged()
	}

	m.mu.Lock()
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
	routeIdentity string,
	generation openssh.Generation,
) error {
	m.mu.Lock()
	route := m.routes[identity]
	active := route != nil && route.generation == generation && route.leases > 0
	m.mu.Unlock()
	if !active {
		if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
			failure := cleanupFailed(err)
			m.emitFailure(routeIdentity, generation, failure)
			return failure
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
	defer unlockIdentity()

	m.mu.Lock()
	route := m.routes[identity]
	if route == nil || route.generation != generation {
		m.mu.Unlock()
		return connectionChanged()
	}
	route.leases--
	if route.leases > 0 {
		m.mu.Unlock()
		return nil
	}
	if m.idleTimeout > 0 {
		route.idle = time.AfterFunc(m.idleTimeout, func() {
			m.disconnectIdle(identity, generation)
		})
		m.mu.Unlock()
		return nil
	}
	delete(m.routes, identity)
	routeIdentity := route.routeIdentity
	m.mu.Unlock()
	if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
		failure := cleanupFailed(err)
		m.emitFailure(routeIdentity, generation, failure)
		return failure
	}
	m.emit(Event{
		RouteIdentity: routeIdentity,
		Generation:    uint64(generation),
		State:         EventStateDisconnected,
	})
	return nil
}

func (m *Manager) disconnectIdle(identity string, generation openssh.Generation) {
	unlockIdentity := m.lockIdentity(identity)
	defer unlockIdentity()

	m.mu.Lock()
	route := m.routes[identity]
	if route == nil || route.generation != generation || route.leases != 0 {
		m.mu.Unlock()
		return
	}
	delete(m.routes, identity)
	routeIdentity := route.routeIdentity
	m.mu.Unlock()
	if err := m.persistent.Disconnect(context.Background(), identity); err != nil {
		m.emitFailure(routeIdentity, generation, cleanupFailed(err))
		return
	}
	m.emit(Event{
		RouteIdentity: routeIdentity,
		Generation:    uint64(generation),
		State:         EventStateDisconnected,
	})
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
