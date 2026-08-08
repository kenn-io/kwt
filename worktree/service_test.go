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
)

type testCache struct {
	mu     sync.Mutex
	result Result
	ok     bool
}

func (c *testCache) Current() (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.ok
}

func (c *testCache) Store(result Result) error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
