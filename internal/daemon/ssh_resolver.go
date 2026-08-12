package daemon

import (
	"context"
	"os"
	"strings"
	"time"

	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	internalssh "go.kenn.io/kwt/internal/ssh"
)

type configuredSSHResolver struct {
	home        string
	environment func() []string
	build       func(internalssh.ResolverOptions) SSHResolver
}

func newConfiguredSSHResolver(home string, now func() time.Time) *configuredSSHResolver {
	return &configuredSSHResolver{
		home:        home,
		environment: os.Environ,
		build: func(options internalssh.ResolverOptions) SSHResolver {
			return internalssh.NewService(internalssh.ServiceOptions{
				Resolver: internalssh.NewResolver(options),
				Now:      now,
			})
		},
	}
}

func (r *configuredSSHResolver) Resolve(
	ctx context.Context,
	request kwt.SSHResolveRequest,
) (kwt.SSHRouteSnapshot, error) {
	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(
		r.home,
		func(path string) (string, error) { return path, nil },
	)
	if err != nil {
		return kwt.SSHRouteSnapshot{}, err
	}
	tokenEnvironment := strings.TrimSpace(snapshot.Config.Fleet.TokenEnv)
	protectedNames := credentials.ProtectedNames(snapshot.Config)
	environment := credentials.StripEnvironment(r.environment(), protectedNames)
	resolverProtectedNames := make([]string, 0, 1)
	if tokenEnvironment != "" {
		resolverProtectedNames = append(resolverProtectedNames, tokenEnvironment)
	}
	resolver := r.build(internalssh.ResolverOptions{
		Environment:    environment,
		ProtectedNames: resolverProtectedNames,
	})
	return resolver.Resolve(ctx, request)
}
