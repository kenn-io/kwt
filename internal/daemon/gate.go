package daemon

import (
	"context"
	"sync"
	"time"

	"go.kenn.io/kwt/service"
)

type ReservationKind uint8

const (
	ReservationWork ReservationKind = iota + 1
	ReservationLease
)

type DrainResult uint8

const (
	DrainReleased DrainResult = iota + 1
	DrainDeadline
	DrainCanceled
)

type GateSnapshot struct {
	Draining       bool
	DrainDeadline  time.Time
	ActiveWork     int
	ActiveLeases   int
	LastActivityAt time.Time
}

type Gate struct {
	mu             sync.Mutex
	lastActivity   time.Time
	draining       bool
	drainDeadline  time.Time
	activeWork     int
	activeLeases   int
	reservationSeq uint64
	reservations   map[uint64]ReservationKind
	changed        chan struct{}
}

func NewGate(now time.Time) *Gate {
	return &Gate{
		lastActivity: now,
		reservations: make(map[uint64]ReservationKind),
		changed:      make(chan struct{}),
	}
}

func (g *Gate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

func (g *Gate) Touch(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining || !now.After(g.lastActivity) {
		return
	}
	g.lastActivity = now
	g.notifyLocked()
}

func (g *Gate) Reserve(kind ReservationKind, now time.Time) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return nil, service.NewError(
			service.DaemonDraining,
			"daemon is draining",
			true,
			map[string]any{"drain_deadline": g.drainDeadline},
			nil,
		)
	}
	g.reservationSeq++
	id := g.reservationSeq
	g.reservations[id] = kind
	if kind == ReservationLease {
		g.activeLeases++
	} else {
		g.activeWork++
	}
	if now.After(g.lastActivity) {
		g.lastActivity = now
	}
	g.notifyLocked()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			kind, ok := g.reservations[id]
			if !ok {
				return
			}
			delete(g.reservations, id)
			if kind == ReservationLease {
				g.activeLeases--
			} else {
				g.activeWork--
			}
			g.notifyLocked()
		})
	}, nil
}

func (g *Gate) BeginDrain(deadline time.Time) GateSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.draining {
		g.draining = true
		g.drainDeadline = deadline
		g.notifyLocked()
	}
	return g.snapshotLocked()
}

func (g *Gate) Snapshot() GateSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snapshotLocked()
}

func (g *Gate) snapshotLocked() GateSnapshot {
	return GateSnapshot{
		Draining:       g.draining,
		DrainDeadline:  g.drainDeadline,
		ActiveWork:     g.activeWork,
		ActiveLeases:   g.activeLeases,
		LastActivityAt: g.lastActivity,
	}
}

func (g *Gate) ShouldStopForIdle(now time.Time, idleTimeout time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return idleTimeout > 0 && !g.draining && g.activeWork == 0 &&
		g.activeLeases == 0 && !now.Before(g.lastActivity.Add(idleTimeout))
}

func (g *Gate) WaitForDrain(ctx context.Context, now time.Time) DrainResult {
	for {
		g.mu.Lock()
		if g.draining && g.activeWork == 0 && g.activeLeases == 0 {
			g.mu.Unlock()
			return DrainReleased
		}
		deadline := g.drainDeadline
		changed := g.changed
		g.mu.Unlock()
		if !deadline.After(now) {
			return DrainDeadline
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ctx.Done():
			timer.Stop()
			return DrainCanceled
		case <-changed:
			timer.Stop()
			now = time.Now()
		case <-timer.C:
			return DrainDeadline
		}
	}
}

func (g *Gate) WaitForRelease(ctx context.Context) bool {
	for {
		g.mu.Lock()
		if g.activeWork == 0 && g.activeLeases == 0 {
			g.mu.Unlock()
			return true
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
}
