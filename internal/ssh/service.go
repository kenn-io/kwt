package ssh

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kwt/service"
)

type observationResolver interface {
	Resolve(context.Context, ResolveRequest) (routeObservation, error)
}

type leaseProvider interface {
	Acquire(
		context.Context,
		LeaseRequest,
		func(context.Context) (RouteSnapshot, error),
	) (Lease, error)
}

type ServiceOptions struct {
	Resolver observationResolver
	Leases   leaseProvider
	Now      func() time.Time
}

type Service struct {
	resolver observationResolver
	leases   leaseProvider
	now      func() time.Time
}

func NewService(options ServiceOptions) *Service {
	resolver := options.Resolver
	if resolver == nil {
		resolver = NewResolver(ResolverOptions{})
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Service{resolver: resolver, leases: options.Leases, now: now}
}

func (s *Service) Acquire(ctx context.Context, request LeaseRequest) (Lease, error) {
	resolve := func(ctx context.Context) (RouteSnapshot, error) {
		return s.Resolve(ctx, ResolveRequest{Target: request.Snapshot.LogicalTarget})
	}
	current, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	if !sameRoute(request.Snapshot, current) {
		return nil, configurationChanged()
	}
	if s.leases == nil {
		return nil, service.NewError(
			service.Internal, "internal failure", false, nil,
			errors.New("SSH lease provider is unavailable"),
		)
	}
	return s.leases.Acquire(ctx, request, resolve)
}

func (s *Service) Resolve(
	ctx context.Context,
	request ResolveRequest,
) (RouteSnapshot, error) {
	observation, err := s.resolver.Resolve(ctx, request)
	if err != nil {
		return RouteSnapshot{}, normalizeSSHError(err)
	}
	targets := make([]ResolvedTarget, 0, len(observation.route))
	for _, observed := range observation.route {
		projected, projectErr := projectConfig(
			observed.Config,
			observation.identityFiles[observed.Target],
		)
		if projectErr != nil {
			return RouteSnapshot{}, routeUnreviewable(projectErr)
		}
		logical := targetFromOpenSSH(observed.Target)
		effective := effectiveTarget(logical, observed.Config.User, observed.Config.Hostname, observed.Config.Port)
		targets = append(targets, ResolvedTarget{
			LogicalTarget:         logical,
			EffectiveTarget:       effective,
			DisplayTarget:         effective.Display(),
			HostKeyAlias:          observed.Config.HostKeyAlias,
			StrictHostKeyChecking: observed.Config.StrictHostKeyChecking,
			ForwardAgent:          projected.ForwardAgent,
			Projection: ExecutionProjection{
				Arguments:     projected.Arguments,
				PrivateConfig: projected.PrivateConfig,
			},
		})
	}
	return RouteSnapshot{
		LogicalTarget: request.Target,
		Targets:       targets,
		RouteIdentity: routeIdentity(
			projectionPolicyV1,
			observation.route,
			observation.identityFiles,
		),
		ProjectionPolicy: projectionPolicyV1,
		ObservedAt:       s.now().UTC(),
	}, nil
}

func effectiveTarget(logical Target, user, hostname string, port int) Target {
	result := logical
	if user != "" {
		result.User = user
	}
	if hostname != "" {
		result.Hostname = hostname
	}
	if port != 0 {
		result.Port = port
	}
	return result
}
