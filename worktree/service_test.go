package worktree

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

type testCache struct {
	mu       sync.Mutex
	result   Result
	ok       bool
	storeErr error
}

func (c *testCache) Current() (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.ok
}

func (c *testCache) Store(result Result) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return c.storeErr
	}
	c.result, c.ok = result, true
	return nil
}

type testSource struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	result  Result
	err     error
}

func (s *testSource) Load(ctx context.Context, _ Request) (Result, error) {
	s.calls.Add(1)
	if s.started != nil {
		select {
		case <-s.started:
		default:
			close(s.started)
		}
	}
	if s.release != nil {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-s.release:
		}
	}
	return s.result, s.err
}

func (*testSource) ApproveConfig(context.Context, ConfigApproval) error { return nil }

type sourceFunc func(context.Context, Request) (Result, error)

func (f sourceFunc) Load(ctx context.Context, request Request) (Result, error) {
	return f(ctx, request)
}

func (sourceFunc) ApproveConfig(context.Context, ConfigApproval) error { return nil }

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestServiceCachedDashboardReportsRefreshState(t *testing.T) {
	cache := &testCache{ok: true, result: Result{Snapshot: Snapshot{Entries: []Entry{{Path: "/old"}}}}}
	source := &testSource{started: make(chan struct{}), release: make(chan struct{})}
	service := NewInventoryService(ServiceOptions{Source: source, Cache: cache})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = service.Query(context.Background(), Request{View: ViewDashboard, RequireCurrent: true})
	}()
	<-source.started
	got, err := service.Query(context.Background(), Request{View: ViewDashboard})
	require.NoError(t, err)
	assert.Equal(t, Refreshing, got.Freshness)
	assert.Equal(t, "/old", got.Snapshot.Entries[0].Path)
	close(source.release)
	<-done
}

func TestServiceCurrentRefreshIsSingleFlight(t *testing.T) {
	source := &testSource{
		started: make(chan struct{}), release: make(chan struct{}),
		result: Result{Snapshot: Snapshot{Entries: []Entry{{Path: "/new"}}}},
	}
	service := NewInventoryService(ServiceOptions{Source: source, Cache: &testCache{}})
	var wait sync.WaitGroup
	errorsOut := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := service.Query(context.Background(), Request{View: ViewDashboard, RequireCurrent: true})
			errorsOut <- err
		}()
	}
	<-source.started
	time.Sleep(20 * time.Millisecond)
	close(source.release)
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), source.calls.Load())
}

func TestServiceCallerCancellationDoesNotCancelSharedRefresh(t *testing.T) {
	source := &testSource{
		started: make(chan struct{}), release: make(chan struct{}),
		result: Result{Snapshot: Snapshot{Entries: []Entry{{Path: "/fresh"}}}},
	}
	service := NewInventoryService(ServiceOptions{Source: source, Cache: &testCache{}})
	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Query(firstContext, Request{View: ViewDashboard, RequireCurrent: true})
		firstDone <- err
	}()
	<-source.started
	secondContext := &observedDoneContext{
		Context: context.Background(), observed: make(chan struct{}),
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.Query(secondContext, Request{View: ViewDashboard, RequireCurrent: true})
		secondDone <- err
	}()
	<-secondContext.observed
	cancelFirst()
	require.ErrorIs(t, <-firstDone, context.Canceled)
	close(source.release)

	require.NoError(t, <-secondDone)
	assert.Equal(t, int32(1), source.calls.Load())
}

func TestServiceCanceledWaiterReturnsBeforeSharedRefreshFinishes(t *testing.T) {
	source := &testSource{
		started: make(chan struct{}), release: make(chan struct{}),
		result: Result{Snapshot: Snapshot{Entries: []Entry{{Path: "/fresh"}}}},
	}
	service := NewInventoryService(ServiceOptions{Source: source, Cache: &testCache{}})
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Query(context.Background(), Request{View: ViewDashboard, RequireCurrent: true})
		firstDone <- err
	}()
	<-source.started
	waiterBase, cancelWaiter := context.WithCancel(context.Background())
	waiterContext := &observedDoneContext{
		Context: waiterBase, observed: make(chan struct{}),
	}
	waiterDone := make(chan error, 1)
	go func() {
		_, err := service.Query(waiterContext, Request{View: ViewDashboard, RequireCurrent: true})
		waiterDone <- err
	}()
	<-waiterContext.observed
	cancelWaiter()

	var waiterErr error
	returnedBeforeRefresh := false
	select {
	case waiterErr = <-waiterDone:
		returnedBeforeRefresh = true
	case <-time.After(time.Second):
	}
	close(source.release)
	require.NoError(t, <-firstDone)
	if !returnedBeforeRefresh {
		waiterErr = <-waiterDone
	}
	assert.True(t, returnedBeforeRefresh)
	require.ErrorIs(t, waiterErr, context.Canceled)
}

func TestServiceFailedRefreshKeepsLastKnownGood(t *testing.T) {
	cache := &testCache{ok: true, result: Result{Snapshot: Snapshot{Entries: []Entry{{Path: "/old"}}}}}
	service := NewInventoryService(ServiceOptions{
		Source: &testSource{err: errors.New("scan failed")}, Cache: cache,
		Now: func() time.Time { return time.Unix(10, 0) },
	})

	_, err := service.Query(context.Background(), Request{View: ViewDashboard, RequireCurrent: true})
	require.Error(t, err)
	cached, ok := cache.Current()
	require.True(t, ok)
	assert.Equal(t, "/old", cached.Snapshot.Entries[0].Path)
}

func TestServiceFreshDashboardSurvivesCacheWriteFailure(t *testing.T) {
	cacheErr := errors.New("cache is read-only")
	service := NewInventoryService(ServiceOptions{
		Source: &testSource{result: Result{Snapshot: Snapshot{Entries: []Entry{{Path: "/new"}}}}},
		Cache:  &testCache{storeErr: cacheErr},
		Now:    func() time.Time { return time.Unix(10, 0) },
	})

	result, err := service.Query(context.Background(), Request{View: ViewDashboard, RequireCurrent: true})

	require.NoError(t, err)
	assert.Equal(t, Fresh, result.Freshness)
	require.Len(t, result.Snapshot.Entries, 1)
	assert.Equal(t, "/new", result.Snapshot.Entries[0].Path)
	require.NotNil(t, result.RefreshError)
	assert.Contains(t, result.RefreshError.Message, cacheErr.Error())
}

func TestServiceResultsDoNotShareEffectiveConfig(t *testing.T) {
	cache := &testCache{}
	service := NewInventoryService(ServiceOptions{
		Source: &testSource{result: Result{Snapshot: Snapshot{Config: &models.Config{
			Worktree: models.WorktreeConfig{BaseDir: "/original"},
			Agents:   map[string]string{"agent": "command"},
			Layouts: models.LayoutsConfig{Presets: []models.Layout{{
				Name: "layout", Panes: []string{"original"},
			}}},
		}}}},
		Cache: cache,
	})

	result, err := service.Query(context.Background(), Request{
		View: ViewDashboard, RequireCurrent: true,
	})
	require.NoError(t, err)
	result.Snapshot.Config.Worktree.BaseDir = "/mutated"
	result.Snapshot.Config.Agents["agent"] = "changed"
	result.Snapshot.Config.Layouts.Presets[0].Panes[0] = "changed"

	cached, ok := cache.Current()
	require.True(t, ok)
	require.NotNil(t, cached.Snapshot.Config)
	assert.Equal(t, "/original", cached.Snapshot.Config.Worktree.BaseDir)
	assert.Equal(t, "command", cached.Snapshot.Config.Agents["agent"])
	assert.Equal(t, "original", cached.Snapshot.Config.Layouts.Presets[0].Panes[0])
}

func TestServiceNewestDashboardRefreshOwnsSharedCache(t *testing.T) {
	oldStarted, oldRelease := make(chan struct{}), make(chan struct{})
	newStarted, newRelease := make(chan struct{}), make(chan struct{})
	source := sourceFunc(func(ctx context.Context, request Request) (Result, error) {
		started, release, path := oldStarted, oldRelease, "/old"
		if request.LaunchDirectory == "/new" {
			started, release, path = newStarted, newRelease, "/new"
		}
		close(started)
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-release:
			return Result{Snapshot: Snapshot{Entries: []Entry{{Path: path}}}}, nil
		}
	})
	cache := &testCache{}
	service := NewInventoryService(ServiceOptions{Source: source, Cache: cache})
	oldDone, newDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := service.Query(context.Background(), Request{
			View: ViewDashboard, LaunchDirectory: "/old", RequireCurrent: true,
		})
		oldDone <- err
	}()
	<-oldStarted
	go func() {
		_, err := service.Query(context.Background(), Request{
			View: ViewDashboard, LaunchDirectory: "/new", RequireCurrent: true,
		})
		newDone <- err
	}()
	<-newStarted

	close(newRelease)
	require.NoError(t, <-newDone)
	close(oldRelease)
	require.NoError(t, <-oldDone)
	cached, ok := cache.Current()
	require.True(t, ok)
	require.Len(t, cached.Snapshot.Entries, 1)
	assert.Equal(t, "/new", cached.Snapshot.Entries[0].Path)
}

func TestServiceReportsRefreshingUntilEveryDashboardRefreshFinishes(t *testing.T) {
	oneStarted, oneRelease := make(chan struct{}), make(chan struct{})
	twoStarted, twoRelease := make(chan struct{}), make(chan struct{})
	source := sourceFunc(func(ctx context.Context, request Request) (Result, error) {
		started, release := oneStarted, oneRelease
		if request.LaunchDirectory == "/two" {
			started, release = twoStarted, twoRelease
		}
		close(started)
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-release:
			return Result{}, nil
		}
	})
	cache := &testCache{ok: true, result: Result{Snapshot: Snapshot{Entries: []Entry{{Path: "/cached"}}}}}
	service := NewInventoryService(ServiceOptions{Source: source, Cache: cache})
	oneDone, twoDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := service.Query(context.Background(), Request{
			View: ViewDashboard, LaunchDirectory: "/one", RequireCurrent: true,
		})
		oneDone <- err
	}()
	<-oneStarted
	go func() {
		_, err := service.Query(context.Background(), Request{
			View: ViewDashboard, LaunchDirectory: "/two", RequireCurrent: true,
		})
		twoDone <- err
	}()
	<-twoStarted

	close(twoRelease)
	require.NoError(t, <-twoDone)
	cached, err := service.Query(context.Background(), Request{View: ViewDashboard})
	require.NoError(t, err)
	assert.Equal(t, Refreshing, cached.Freshness)

	close(oneRelease)
	require.NoError(t, <-oneDone)
}
