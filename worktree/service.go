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
	source              Source
	cache               Cache
	now                 func() time.Time
	group               singleflight.Group
	publishMu           sync.Mutex
	mu                  sync.RWMutex
	active              int
	dashboardGeneration uint64
	last                *Diagnostic
}

func NewInventoryService(options ServiceOptions) *Service {
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{source: options.Source, cache: options.Cache, now: options.Now}
}

func (s *Service) Query(ctx context.Context, request Request) (Result, error) {
	if request.View == ViewDashboard && !request.RequireCurrent && s.cache != nil {
		if cached, ok := s.cache.Current(); ok {
			s.mu.RLock()
			if s.active > 0 {
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
		var generation uint64
		if request.View == ViewDashboard {
			s.mu.Lock()
			s.active++
			s.dashboardGeneration++
			generation = s.dashboardGeneration
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				s.active--
				s.mu.Unlock()
			}()
		}
		result, loadErr := s.source.Load(ctx, request)
		if loadErr != nil {
			if request.View == ViewDashboard {
				s.setDashboardDiagnostic(generation, &Diagnostic{
					At: s.now(), Message: boundedDiagnostic(loadErr),
				})
			}
			return Result{}, loadErr
		}
		result.Freshness = Fresh
		result.ObservedAt = s.now()
		result.RefreshError = nil
		if request.View == ViewDashboard {
			result.RefreshError = s.publishDashboard(generation, result)
		}
		return cloneResult(result), nil
	})
	if err != nil {
		return Result{}, err
	}
	return cloneResult(value.(Result)), nil
}

func (s *Service) publishDashboard(generation uint64, result Result) *Diagnostic {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if !s.isLatestDashboard(generation) {
		return nil
	}
	var diagnostic *Diagnostic
	if s.cache != nil {
		if err := s.cache.Store(result); err != nil {
			diagnostic = &Diagnostic{At: s.now(), Message: boundedDiagnostic(err)}
		}
	}
	s.mu.Lock()
	if generation == s.dashboardGeneration {
		s.last = cloneDiagnostic(diagnostic)
	}
	s.mu.Unlock()
	return diagnostic
}

func (s *Service) setDashboardDiagnostic(generation uint64, diagnostic *Diagnostic) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == s.dashboardGeneration {
		s.last = cloneDiagnostic(diagnostic)
	}
}

func (s *Service) isLatestDashboard(generation uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return generation == s.dashboardGeneration
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
