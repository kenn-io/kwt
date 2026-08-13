package kwt

import (
	"context"
	"os"
	"time"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	internalssh "go.kenn.io/kwt/internal/ssh"
)

type (
	SSHTarget              = internalssh.Target
	SSHResolveRequest      = internalssh.ResolveRequest
	SSHExecutionProjection = internalssh.ExecutionProjection
	SSHResolvedTarget      = internalssh.ResolvedTarget
	SSHRouteSnapshot       = internalssh.RouteSnapshot
)

const SSHProjectionPolicyV1 = internalssh.ProjectionPolicyV1

type SSHServiceOptions struct {
	Home           string
	Environment    func() []string
	ProtectedNames []string
	Now            func() time.Time
}

type sshSnapshotResolver interface {
	Resolve(context.Context, SSHResolveRequest) (SSHRouteSnapshot, error)
}

type SSHService struct {
	home           string
	environment    func() []string
	protectedNames []string
	build          func(internalssh.ResolverOptions) sshSnapshotResolver
}

func NewSSHService(options SSHServiceOptions) *SSHService {
	environment := options.Environment
	if environment == nil {
		environment = os.Environ
	}
	return &SSHService{
		home:           options.Home,
		environment:    environment,
		protectedNames: append([]string(nil), options.ProtectedNames...),
		build: func(resolverOptions internalssh.ResolverOptions) sshSnapshotResolver {
			return internalssh.NewService(internalssh.ServiceOptions{
				Resolver: internalssh.NewResolver(resolverOptions),
				Now:      options.Now,
			})
		},
	}
}

func (s *SSHService) Resolve(
	ctx context.Context,
	request SSHResolveRequest,
) (SSHRouteSnapshot, error) {
	var (
		snapshot *config.GlobalSnapshot
		err      error
	)
	if s.home == "" {
		snapshot, err = config.LoadGlobalSnapshot()
	} else {
		snapshot, err = config.LoadGlobalSnapshotAtWithExpansion(
			s.home,
			func(path string) (string, error) { return path, nil },
		)
	}
	if err != nil {
		return SSHRouteSnapshot{}, err
	}
	protectedNames := append(credentials.ProtectedNames(snapshot.Config), s.protectedNames...)
	workingDirectory := request.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory, err = os.Getwd()
		if err != nil {
			return SSHRouteSnapshot{}, err
		}
	}
	environment := request.Environment
	if environment == nil {
		environment = s.environment()
	}
	environment = credentials.StripEnvironment(environment, protectedNames)
	resolver := s.build(internalssh.ResolverOptions{
		WorkingDirectory: workingDirectory,
		Environment:      environment,
		ProtectedNames:   protectedNames,
	})
	return resolver.Resolve(ctx, request)
}
