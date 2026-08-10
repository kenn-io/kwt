package lifecycle

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"

	"go.kenn.io/kwt/pkg/models"
	"golang.org/x/sync/singleflight"
)

type InventoryServiceOptions struct {
	Source         Source
	Cache          Cache
	Now            func() time.Time
	Context        context.Context
	RefreshTimeout time.Duration
}

type InventoryService struct {
	source              Source
	cache               Cache
	now                 func() time.Time
	context             context.Context
	refreshTimeout      time.Duration
	group               singleflight.Group
	publishMu           sync.Mutex
	mu                  sync.RWMutex
	active              int
	dashboardGeneration uint64
	last                *Diagnostic
}

func NewInventoryService(options InventoryServiceOptions) *InventoryService {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Context == nil {
		options.Context = context.Background()
	}
	if options.RefreshTimeout <= 0 {
		options.RefreshTimeout = DefaultRefreshTimeout
	}
	return &InventoryService{
		source: options.Source, cache: options.Cache, now: options.Now,
		context: options.Context, refreshTimeout: options.RefreshTimeout,
	}
}

func (s *InventoryService) Query(ctx context.Context, request Request) (Result, error) {
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
	result := s.group.DoChan(string(encoded), func() (any, error) {
		refreshContext, cancel := context.WithTimeout(s.context, s.refreshTimeout)
		defer cancel()
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
		result, loadErr := s.source.Load(refreshContext, request)
		loadErr = classifyInventoryError(loadErr)
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
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case outcome := <-result:
		if outcome.Err != nil {
			return Result{}, outcome.Err
		}
		return cloneResult(outcome.Val.(Result)), nil
	}
}

func (s *InventoryService) publishDashboard(generation uint64, result Result) *Diagnostic {
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

func (s *InventoryService) setDashboardDiagnostic(generation uint64, diagnostic *Diagnostic) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == s.dashboardGeneration {
		s.last = cloneDiagnostic(diagnostic)
	}
}

func (s *InventoryService) isLatestDashboard(generation uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return generation == s.dashboardGeneration
}

func (s *InventoryService) ApproveConfig(ctx context.Context, approval ConfigApproval) error {
	return s.source.ApproveConfig(ctx, approval)
}

func cloneResult(result Result) Result {
	copy := result
	copy.Snapshot.Config = cloneConfig(result.Snapshot.Config)
	copy.Snapshot.Projects = cloneSlice(result.Snapshot.Projects)
	copy.Snapshot.Entries = cloneSlice(result.Snapshot.Entries)
	copy.Snapshot.LaunchEntries = cloneSlice(result.Snapshot.LaunchEntries)
	copy.Snapshot.Workspaces = cloneSlice(result.Snapshot.Workspaces)
	copy.Notes = cloneSlice(result.Notes)
	copy.RefreshError = cloneDiagnostic(result.RefreshError)
	return copy
}

func cloneConfig(value *models.Config) *models.Config {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Agents = maps.Clone(value.Agents)
	copy.Projects = slices.Clone(value.Projects)
	copy.Workspaces = slices.Clone(value.Workspaces)
	copy.RepositorySettings = slices.Clone(value.RepositorySettings)
	for index := range copy.RepositorySettings {
		copy.RepositorySettings[index].SetupCommands = slices.Clone(
			value.RepositorySettings[index].SetupCommands,
		)
		copy.RepositorySettings[index].CopyFiles = slices.Clone(
			value.RepositorySettings[index].CopyFiles,
		)
	}
	copy.Layouts.Presets = slices.Clone(value.Layouts.Presets)
	for index := range copy.Layouts.Presets {
		copy.Layouts.Presets[index].Panes = slices.Clone(value.Layouts.Presets[index].Panes)
	}
	copy.Naming.SanitizeChars = maps.Clone(value.Naming.SanitizeChars)
	return &copy
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
