package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/kit/openssh"
	"go.kenn.io/kwt/service"
)

const leaseRollbackTimeout = 5 * time.Second

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
	idleCleanupMu        sync.Mutex
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
	hopDepth         int
	upstreamIdentity string
	idleDeadline     time.Time
}

type routeCleanupTarget struct {
	identity   string
	generation openssh.Generation
	depth      int
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
	if len(request.Snapshot.Targets) == 0 {
		return nil, routeUnreviewable(errors.New("SSH lifecycle requires a resolved target"))
	}
	leases := make([]Lease, 0, len(request.Snapshot.Targets))
	previousIdentity := ""
	for index, originalTarget := range request.Snapshot.Targets {
		target := originalTarget
		if index > 0 {
			previous := leases[index-1]
			if previous.Mode() != LeaseModeMultiplexed {
				return nil, errors.Join(
					releaseAcquiredLeases(ctx, leases),
					routeUnreviewable(errors.New("masterless ProxyJump routes are unsupported")),
				)
			}
			arguments, argumentErr := previous.Arguments(ctx)
			if argumentErr != nil {
				return nil, errors.Join(releaseAcquiredLeases(ctx, leases), argumentErr)
			}
			target = targetWithProxy(target, request.Snapshot.Targets[index-1], arguments)
		}
		identity := managerIdentityForTarget(
			request.Snapshot, target, index, previousIdentity, request.Environment,
		)
		lease, acquireErr := m.acquireTarget(
			ctx, request, target, identity, previousIdentity, index,
			index == len(request.Snapshot.Targets)-1, resolve,
		)
		if acquireErr != nil {
			return nil, errors.Join(releaseAcquiredLeases(ctx, leases), acquireErr)
		}
		leases = append(leases, lease)
		previousIdentity = identity
	}
	if len(leases) == 1 {
		return leases[0], nil
	}
	return &routeLease{leases: leases}, nil
}

func (m *Manager) acquireTarget(
	ctx context.Context,
	request LeaseRequest,
	target ResolvedTarget,
	identity string,
	upstreamIdentity string,
	hopDepth int,
	terminal bool,
	resolve func(context.Context) (RouteSnapshot, error),
) (Lease, error) {
	runner, err := m.runner(request, target)
	if err != nil {
		return nil, connectionFailed(err)
	}
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
	closed := m.closed
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
	current, err := resolve(ctx)
	if err != nil {
		event, cleanupErr := m.disconnectUnleased(
			identity, generation, request.Snapshot.RouteIdentity, target.ForwardAgent,
			upstreamIdentity, hopDepth,
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
			upstreamIdentity, hopDepth,
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
	closed = m.closed
	m.mu.Unlock()
	if closed {
		event, cleanupErr := m.disconnectUnleased(
			identity, generation, request.Snapshot.RouteIdentity, target.ForwardAgent,
			upstreamIdentity, hopDepth,
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
	sessionProjection := ExecutionProjection{Arguments: []string{"-F", os.DevNull}}
	if terminal {
		sessionProjection = multiplexedSessionProjection(target.Projection)
	}
	sessionArguments, sessionCleanup, err := materializeProjection(
		m.privateDirectory, sessionProjection,
	)
	if err != nil {
		event, cleanupErr := m.disconnectUnleased(
			identity, generation, request.Snapshot.RouteIdentity, target.ForwardAgent,
			upstreamIdentity, hopDepth,
		)
		releaseIdentity()
		m.emitOptional(event)
		if cleanupErr != nil {
			m.emitFailure(request.Snapshot.RouteIdentity, generation, cleanupErr)
		}
		return nil, errors.Join(connectionFailed(err), cleanupErr)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		var projectionCleanupErr error
		if sessionCleanup != nil {
			projectionCleanupErr = sessionCleanup()
		}
		event, cleanupErr := m.disconnectUnleased(
			identity, generation, request.Snapshot.RouteIdentity, target.ForwardAgent,
			upstreamIdentity, hopDepth,
		)
		if projectionCleanupErr != nil || cleanupErr != nil {
			failure := cleanupFailed(errors.Join(projectionCleanupErr, cleanupErr))
			releaseIdentity()
			m.emitFailure(request.Snapshot.RouteIdentity, generation, failure)
			return nil, failure
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
			routeIdentity:    request.Snapshot.RouteIdentity,
			generation:       generation,
			forwardAgent:     target.ForwardAgent,
			hopDepth:         hopDepth,
			upstreamIdentity: upstreamIdentity,
		}
		m.routes[identity] = route
	}
	if route.idle != nil {
		route.idle.Stop()
		route.idle = nil
	}
	route.idleDeadline = time.Time{}
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
		routeIdentity:    request.Snapshot.RouteIdentity,
		sessionArguments: sessionArguments, sessionCleanup: sessionCleanup,
	}, nil
}

type routeLease struct {
	leases         []Lease
	released       atomic.Bool
	releaseStarted atomic.Bool
	releaseMu      sync.Mutex
}

func (l *routeLease) Mode() LeaseMode { return l.leases[len(l.leases)-1].Mode() }

func (l *routeLease) Generation() uint64 {
	return l.leases[len(l.leases)-1].Generation()
}

func (l *routeLease) Arguments(ctx context.Context) ([]string, error) {
	if l.releaseStarted.Load() {
		return nil, connectionChanged()
	}
	return l.leases[len(l.leases)-1].Arguments(ctx)
}

func (l *routeLease) Touch() error {
	if l.releaseStarted.Load() {
		return connectionChanged()
	}
	for _, lease := range l.leases {
		if err := lease.Touch(); err != nil {
			return err
		}
	}
	return nil
}

func (l *routeLease) Release(ctx context.Context) error {
	l.releaseMu.Lock()
	defer l.releaseMu.Unlock()
	if l.released.Load() {
		return nil
	}
	l.releaseStarted.Store(true)
	if err := releaseLeases(ctx, l.leases); err != nil {
		return err
	}
	l.released.Store(true)
	return nil
}

func releaseLeases(ctx context.Context, leases []Lease) error {
	var releaseErr error
	for index := len(leases) - 1; index >= 0; index-- {
		releaseErr = errors.Join(releaseErr, leases[index].Release(ctx))
	}
	return releaseErr
}

func releaseAcquiredLeases(ctx context.Context, leases []Lease) error {
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), leaseRollbackTimeout,
	)
	defer cancel()
	return releaseLeases(cleanupCtx, leases)
}

func (m *Manager) disconnectUnleased(
	identity string,
	generation openssh.Generation,
	routeIdentity string,
	forwardAgent bool,
	upstreamIdentity string,
	hopDepth int,
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
			routeIdentity:    routeIdentity,
			generation:       generation,
			forwardAgent:     forwardAgent,
			hopDepth:         hopDepth,
			upstreamIdentity: upstreamIdentity,
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

func managerIdentityForTarget(
	snapshot RouteSnapshot,
	target ResolvedTarget,
	index int,
	previousIdentity string,
	environment []string,
) string {
	identity := managerIdentity(snapshot)
	if len(snapshot.Targets) > 1 {
		identity += ":hop:" + fmt.Sprint(index)
	}
	if previousIdentity != "" {
		digest := sha256.Sum256([]byte(previousIdentity))
		identity += ":via:" + hex.EncodeToString(digest[:])
	}
	if !target.ForwardAgent {
		return identity
	}
	digest := sha256.New()
	for _, entry := range environment {
		_, _ = digest.Write([]byte(entry))
		_, _ = digest.Write([]byte{0})
	}
	return identity + ":agent:" + hex.EncodeToString(digest.Sum(nil))
}

func targetWithProxy(
	target ResolvedTarget,
	previous ResolvedTarget,
	connectionArguments []string,
) ResolvedTarget {
	target.Projection.Arguments = append(
		append([]string(nil), target.Projection.Arguments...),
		"-o", "ProxyCommand="+proxyCommand(previous.EffectiveTarget, connectionArguments),
	)
	return target
}

func sameRoute(expected, current RouteSnapshot) bool {
	return expected.RouteIdentity == current.RouteIdentity &&
		expected.ProjectionPolicy == current.ProjectionPolicy
}

type managedLease struct {
	manager          *Manager
	identity         string
	routeIdentity    string
	generation       openssh.Generation
	sessionArguments []string
	sessionCleanup   func() error
	released         atomic.Bool
	releaseStarted   atomic.Bool
	releaseMu        sync.Mutex
	countReleased    bool
}

func (l *managedLease) Mode() LeaseMode { return LeaseModeMultiplexed }

func (l *managedLease) Generation() uint64 { return uint64(l.generation) }

func (l *managedLease) Arguments(ctx context.Context) ([]string, error) {
	if l.releaseStarted.Load() {
		return nil, connectionChanged()
	}
	arguments, err := l.manager.persistent.ConnectionArguments(l.identity, l.generation)
	if err != nil {
		return nil, mapManagerError(err)
	}
	arguments = append(append([]string(nil), l.sessionArguments...), arguments...)
	arguments = append(arguments,
		"-o", "BatchMode=yes",
		"-o", "ProxyCommand="+proxyFailureCommand(),
	)
	return arguments, nil
}

func (l *managedLease) Touch() error {
	if l.releaseStarted.Load() || !l.manager.persistent.TouchActivity(l.identity, l.generation) {
		return connectionChanged()
	}
	return nil
}

func (l *managedLease) Release(ctx context.Context) error {
	l.releaseMu.Lock()
	defer l.releaseMu.Unlock()
	if l.released.Load() {
		return nil
	}
	l.releaseStarted.Store(true)
	if l.sessionCleanup != nil {
		if err := l.sessionCleanup(); err != nil {
			failure := cleanupFailed(err)
			l.manager.emitFailure(l.routeIdentity, l.generation, failure)
			return failure
		}
		l.sessionCleanup = nil
	}
	decremented, err := l.manager.release(
		ctx, l.identity, l.generation, !l.countReleased,
	)
	if decremented {
		l.countReleased = true
	}
	if err != nil {
		return err
	}
	l.released.Store(true)
	return nil
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

func (m *Manager) release(
	ctx context.Context,
	identity string,
	generation openssh.Generation,
	decrement bool,
) (bool, error) {
	unlockIdentity, err := m.lockIdentityContext(ctx, identity)
	if err != nil {
		return false, err
	}

	m.mu.Lock()
	route := m.routes[identity]
	if route == nil || route.generation != generation {
		m.mu.Unlock()
		unlockIdentity()
		return false, nil
	}
	decremented := false
	if decrement {
		route.leases--
		decremented = true
	}
	if route.leases > 0 {
		m.mu.Unlock()
		unlockIdentity()
		return decremented, nil
	}
	if m.idleTimeout > 0 && !route.forwardAgent {
		route.idleDeadline = time.Now().Add(m.idleTimeout)
		routeIdentity := route.routeIdentity
		route.idle = time.AfterFunc(m.idleTimeout, func() {
			m.disconnectIdleGroup(routeIdentity)
		})
		m.mu.Unlock()
		unlockIdentity()
		return decremented, nil
	}
	heartbeatDone := m.stopPersistenceHeartbeat(route)
	routeIdentity := route.routeIdentity
	m.mu.Unlock()
	if err := waitPersistenceHeartbeat(ctx, heartbeatDone); err != nil {
		failure := cleanupFailed(err)
		unlockIdentity()
		m.emitFailure(routeIdentity, generation, failure)
		return decremented, failure
	}
	if err := m.persistent.Disconnect(ctx, identity); err != nil {
		failure := cleanupFailed(err)
		unlockIdentity()
		m.emitFailure(routeIdentity, generation, failure)
		return decremented, failure
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
	return decremented, nil
}

func (m *Manager) disconnectIdleGroup(routeIdentity string) {
	m.idleCleanupMu.Lock()
	defer m.idleCleanupMu.Unlock()

	now := time.Now()
	m.mu.Lock()
	targets := make([]routeCleanupTarget, 0)
	for identity, route := range m.routes {
		if route.routeIdentity != routeIdentity || route.leases != 0 ||
			route.idleDeadline.IsZero() || now.Before(route.idleDeadline) {
			continue
		}
		targets = append(targets, routeCleanupTarget{
			identity: identity, generation: route.generation, depth: route.hopDepth,
		})
	}
	m.mu.Unlock()
	sort.Slice(targets, func(left, right int) bool {
		if targets[left].depth != targets[right].depth {
			return targets[left].depth > targets[right].depth
		}
		return targets[left].identity < targets[right].identity
	})
	for _, target := range targets {
		m.disconnectIdle(target.identity, target.generation)
	}
}

func (m *Manager) disconnectIdle(identity string, generation openssh.Generation) {
	unlockIdentity := m.lockIdentity(identity)

	m.mu.Lock()
	route := m.routes[identity]
	if route == nil || route.generation != generation || route.leases != 0 ||
		route.idleDeadline.IsZero() || time.Now().Before(route.idleDeadline) {
		m.mu.Unlock()
		unlockIdentity()
		return
	}
	for _, downstream := range m.routes {
		if downstream.upstreamIdentity == identity {
			m.mu.Unlock()
			unlockIdentity()
			return
		}
	}
	route.idle = nil
	route.idleDeadline = time.Time{}
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
		routeTargets := make([]routeCleanupTarget, 0, len(m.routes))
		for identity, route := range m.routes {
			routeTargets = append(routeTargets, routeCleanupTarget{
				identity: identity, generation: route.generation, depth: route.hopDepth,
			})
			if route.idle != nil {
				route.idle.Stop()
				route.idle = nil
			}
			route.idleDeadline = time.Time{}
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
			routeTargets,
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
	routeTargets []routeCleanupTarget,
	operationIdentities []string,
	masterless []*masterlessLease,
) ([]Event, error) {
	var closeErr error
	events := make([]Event, 0, len(routeTargets)+len(masterless))

	for _, lease := range masterless {
		lease.released.Store(true)
		cleaned, err := lease.cleanupTracked(ctx)
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

	sort.Slice(routeTargets, func(left, right int) bool {
		if routeTargets[left].depth != routeTargets[right].depth {
			return routeTargets[left].depth > routeTargets[right].depth
		}
		return routeTargets[left].identity < routeTargets[right].identity
	})
	for _, target := range routeTargets {
		identity := target.identity
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

func (l *masterlessLease) Release(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.released.Store(true)
	cleaned, err := l.cleanupTracked(ctx)
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

func (l *masterlessLease) cleanupTracked(ctx context.Context) (bool, error) {
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}

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
