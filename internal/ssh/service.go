package ssh

import (
	"context"
	"time"
)

type observationResolver interface {
	Resolve(context.Context, ResolveRequest) (routeObservation, error)
}

type ServiceOptions struct {
	Resolver observationResolver
	Now      func() time.Time
}

type Service struct {
	resolver observationResolver
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
	return &Service{resolver: resolver, now: now}
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
		projected, projectErr := projectConfig(observed.Config)
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
			Projection: ExecutionProjection{
				Arguments:     projected.Arguments,
				PrivateConfig: projected.PrivateConfig,
			},
		})
	}
	return RouteSnapshot{
		LogicalTarget:    request.Target,
		Targets:          targets,
		RouteIdentity:    routeIdentity(projectionPolicyV1, observation.route),
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
