package worktree

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type ServiceOptions struct {
	Source Source
	Cache  Cache
	Now    func() time.Time
}

type Service struct {
	source Source
	cache  Cache
	now    func() time.Time
	group  singleflight.Group
	mu     sync.RWMutex
	active bool
	last   *Diagnostic
}

func NewInventoryService(options ServiceOptions) *Service {
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{source: options.Source, cache: options.Cache, now: options.Now}
}

func (s *Service) Query(ctx context.Context, request Request) (Result, error) {
	if request.View == ViewDashboard && !request.RequireCurrent {
		if cached, ok := s.cache.Current(); ok {
			s.mu.RLock()
			if s.active {
				cached.Freshness = Refreshing
			} else {
				cached.Freshness = Stale
			}
			cached.RefreshError = cloneDiagnostic(s.last)
			s.mu.RUnlock()
			return cloneResult(cached), nil
		}
	}
	encoded, _ := json.Marshal(request)
	value, err, _ := s.group.Do(string(encoded), func() (any, error) {
		if request.View == ViewDashboard {
			s.mu.Lock()
			s.active = true
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				s.active = false
				s.mu.Unlock()
			}()
		}
		result, loadErr := s.source.Load(ctx, request)
		if loadErr != nil {
			s.mu.Lock()
			s.last = &Diagnostic{At: s.now(), Message: boundedDiagnostic(loadErr)}
			s.mu.Unlock()
			return Result{}, loadErr
		}
		result.Freshness = Fresh
		result.ObservedAt = s.now()
		result.RefreshError = nil
		if request.View == ViewDashboard {
			if storeErr := s.cache.Store(result); storeErr != nil {
				result.RefreshError = &Diagnostic{At: s.now(), Message: boundedDiagnostic(storeErr)}
			}
		}
		s.mu.Lock()
		s.last = cloneDiagnostic(result.RefreshError)
		s.mu.Unlock()
		return cloneResult(result), nil
	})
	if err != nil {
		return Result{}, err
	}
	return cloneResult(value.(Result)), nil
}

func (s *Service) ApproveConfig(ctx context.Context, approval ConfigApproval) error {
	return s.source.ApproveConfig(ctx, approval)
}

func cloneResult(result Result) Result {
	copy := result
	copy.Snapshot.Projects = cloneSlice(result.Snapshot.Projects)
	copy.Snapshot.Entries = cloneSlice(result.Snapshot.Entries)
	copy.Snapshot.LaunchEntries = cloneSlice(result.Snapshot.LaunchEntries)
	copy.Snapshot.Workspaces = cloneSlice(result.Snapshot.Workspaces)
	copy.Notes = cloneSlice(result.Notes)
	copy.RefreshError = cloneDiagnostic(result.RefreshError)
	return copy
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	result := make([]T, len(values))
	copy(result, values)
	return result
}

func cloneDiagnostic(diagnostic *Diagnostic) *Diagnostic {
	if diagnostic == nil {
		return nil
	}
	copy := *diagnostic
	return &copy
}
