package ssh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
	servicepkg "go.kenn.io/kwt/service"
)

type fixedObservationResolver struct {
	observation routeObservation
	err         error
	calls       int
}

type fixedLeaseProvider struct {
	lease Lease
	calls int
}

func (p *fixedLeaseProvider) Acquire(
	context.Context,
	LeaseRequest,
	func(context.Context) (RouteSnapshot, error),
) (Lease, error) {
	p.calls++
	return p.lease, nil
}

type fixedLease struct{}

func (fixedLease) Mode() LeaseMode    { return LeaseModeMultiplexed }
func (fixedLease) Generation() uint64 { return 7 }

func (fixedLease) Arguments(context.Context) ([]string, error) {
	return []string{"-S", "socket"}, nil
}

func (fixedLease) Touch() error   { return nil }
func (fixedLease) Release() error { return nil }

func (r *fixedObservationResolver) Resolve(
	context.Context,
	ResolveRequest,
) (routeObservation, error) {
	r.calls++
	return r.observation, r.err
}

func TestServiceBuildsSnapshotFromCompletePrivateObservation(t *testing.T) {
	resolver := &fixedObservationResolver{observation: routeObservation{route: openssh.Route{
		{
			Target: openssh.Target{User: "relay", Hostname: "relay.example.test", Port: 22},
			Config: openssh.EffectiveConfig{
				User: "jump", Hostname: "relay.internal", Port: 2222,
				StrictHostKeyChecking: "yes",
				Options: []openssh.Option{
					{Name: "identityfile", Value: "/keys/relay"},
				},
			},
		},
		{
			Target: openssh.Target{User: "deploy", Hostname: "build.example.test", Port: 22},
			Config: openssh.EffectiveConfig{
				User: "deploy", Hostname: "build.internal", Port: 2200,
				HostKeyAlias: "build-key.example.test", StrictHostKeyChecking: "ask",
				Options: []openssh.Option{
					{Name: "ciphers", Value: "aes256-gcm@openssh.com"},
					{Name: "futureoption", Value: "identity-only"},
				},
			},
		},
	}}}
	observedAt := time.Date(2026, 8, 11, 18, 30, 0, 0, time.FixedZone("CDT", -5*60*60))
	service := NewService(ServiceOptions{
		Resolver: resolver,
		Now:      func() time.Time { return observedAt },
	})

	snapshot, err := service.Resolve(context.Background(), ResolveRequest{
		Target: Target{User: "deploy", Hostname: "build.example.test", Port: 22},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, observedAt.UTC(), snapshot.ObservedAt)
	assert.Equal(t, projectionPolicyV1, snapshot.ProjectionPolicy)
	assert.Len(t, snapshot.RouteIdentity, 64)
	require.Len(t, snapshot.Targets, 2)
	assert.Equal(t, "jump@relay.internal:2222", snapshot.Targets[0].DisplayTarget)
	assert.Equal(t, []string{`IdentityFile "/keys/relay"`}, snapshot.Targets[0].Projection.PrivateConfig)
	assert.Contains(t, snapshot.Targets[1].Projection.Arguments, "Ciphers=aes256-gcm@openssh.com")
	assert.NotContains(t, snapshot.Targets[1].Projection.Arguments, "FutureOption=identity-only")
}

func TestServiceMarksProjectedAgentForwarding(t *testing.T) {
	resolver := &fixedObservationResolver{observation: routeObservation{route: openssh.Route{{
		Target: openssh.Target{Hostname: "build.example.test"},
		Config: openssh.EffectiveConfig{
			Hostname: "build.internal",
			Options:  []openssh.Option{{Name: "forwardagent", Value: "yes"}},
		},
	}}}}
	service := NewService(ServiceOptions{Resolver: resolver})

	snapshot, err := service.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})

	require.NoError(t, err)
	require.Len(t, snapshot.Targets, 1)
	assert.True(t, snapshot.Targets[0].ForwardAgent)
}

func TestServiceFullObservationChangesRouteIdentity(t *testing.T) {
	base := openssh.Route{{
		Target: openssh.Target{Hostname: "build.example.test"},
		Config: openssh.EffectiveConfig{
			Hostname: "build.internal",
			Options:  []openssh.Option{{Name: "futureoption", Value: "one"}},
		},
	}}
	resolve := func(route openssh.Route) RouteSnapshot {
		service := NewService(ServiceOptions{
			Resolver: &fixedObservationResolver{observation: routeObservation{route: route}},
		})
		snapshot, err := service.Resolve(context.Background(), ResolveRequest{
			Target: Target{Hostname: "build.example.test"},
		})
		require.NoError(t, err)
		return snapshot
	}

	first := resolve(base)
	changed := append(openssh.Route(nil), base...)
	changed[0].Config.Options = []openssh.Option{{Name: "futureoption", Value: "two"}}
	second := resolve(changed)
	assert.NotEqual(t, first.RouteIdentity, second.RouteIdentity)
}

func TestServicePreservesResolverCancellation(t *testing.T) {
	resolver := &fixedObservationResolver{err: resolutionFailed(context.Canceled)}
	service := NewService(ServiceOptions{Resolver: resolver})
	_, err := service.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	private := errors.New("private")
	resolver.err = private
	_, err = service.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.Error(t, err)
	assert.Equal(t, "internal failure", err.Error())
	assert.ErrorIs(t, err, private)
}

func TestServiceRejectsChangedRouteBeforeAcquiringLease(t *testing.T) {
	resolver := &fixedObservationResolver{observation: routeObservation{route: openssh.Route{{
		Target: openssh.Target{Hostname: "build.example.test"},
		Config: openssh.EffectiveConfig{Hostname: "first.internal"},
	}}}}
	provider := &fixedLeaseProvider{lease: fixedLease{}}
	service := NewService(ServiceOptions{Resolver: resolver, Leases: provider})
	snapshot, err := service.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.NoError(t, err)
	resolver.observation.route[0].Config.Hostname = "second.internal"

	_, err = service.Acquire(context.Background(), LeaseRequest{Snapshot: snapshot})

	require.Error(t, err)
	assert.Equal(t, 0, provider.calls)
	assert.True(t, servicepkg.IsCode(err, servicepkg.SSHConfigurationChanged))
}

func TestServiceAcquiresLeaseForCurrentRoute(t *testing.T) {
	resolver := &fixedObservationResolver{observation: routeObservation{route: openssh.Route{{
		Target: openssh.Target{Hostname: "build.example.test"},
		Config: openssh.EffectiveConfig{Hostname: "build.internal"},
	}}}}
	provider := &fixedLeaseProvider{lease: fixedLease{}}
	service := NewService(ServiceOptions{Resolver: resolver, Leases: provider})
	snapshot, err := service.Resolve(context.Background(), ResolveRequest{
		Target: Target{Hostname: "build.example.test"},
	})
	require.NoError(t, err)

	lease, err := service.Acquire(context.Background(), LeaseRequest{Snapshot: snapshot})

	require.NoError(t, err)
	assert.Equal(t, 1, provider.calls)
	assert.Equal(t, uint64(7), lease.Generation())
}
