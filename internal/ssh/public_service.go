package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/kit/openssh"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
)

const minimumCrashPersistenceTimeout = time.Minute

type PublicServiceOptions struct {
	Home           string
	Environment    func() []string
	ProtectedNames []string
	// AskpassExecutable must dispatch the root package's RunSSHAskpassHelper
	// before normal startup.
	AskpassExecutable string
	Now               func() time.Time
	OnEvent           func(Event)
}

type snapshotResolver interface {
	Resolve(context.Context, ResolveRequest) (RouteSnapshot, error)
}

type PublicService struct {
	home              string
	environment       func() []string
	protectedNames    []string
	askpassExecutable string
	build             func(ResolverOptions) snapshotResolver
	leases            *Manager
	managerID         string
	managerErr        error
	managerMu         sync.Mutex
	closed            bool
	managerDir        string
	newPersistent     func(string, openssh.PersistentConfig) (PersistentManager, error)
	onEvent           func(Event)
}

type requestContext struct {
	resolver         snapshotResolver
	config           *config.GlobalSnapshot
	workingDirectory string
	environment      []string
}

func NewPublicService(options PublicServiceOptions) *PublicService {
	environment := options.Environment
	if environment == nil {
		environment = os.Environ
	}
	managerID, managerErr := randomManagerID()
	if managerErr == nil && options.AskpassExecutable == "" {
		managerErr = errors.New("SSH askpass helper executable is required")
	}
	if managerErr == nil && !filepath.IsAbs(options.AskpassExecutable) {
		managerErr = errors.New("SSH askpass helper executable must be absolute")
	}
	return &PublicService{
		home:              options.Home,
		environment:       environment,
		protectedNames:    append([]string(nil), options.ProtectedNames...),
		askpassExecutable: options.AskpassExecutable,
		build: func(resolverOptions ResolverOptions) snapshotResolver {
			return NewService(ServiceOptions{
				Resolver: NewResolver(resolverOptions),
				Now:      options.Now,
			})
		},
		managerID:  managerID,
		managerErr: managerErr,
		onEvent:    options.OnEvent,
		newPersistent: func(
			directory string,
			persistentConfig openssh.PersistentConfig,
		) (PersistentManager, error) {
			return openssh.NewPersistentManager(directory, persistentConfig)
		},
	}
}

func randomManagerID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *PublicService) Resolve(
	ctx context.Context,
	request ResolveRequest,
) (RouteSnapshot, error) {
	requestContext, err := s.requestContext(request.WorkingDirectory, request.Environment)
	if err != nil {
		return RouteSnapshot{}, err
	}
	return requestContext.resolver.Resolve(ctx, request)
}

func (s *PublicService) requestContext(
	workingDirectory string,
	environment []string,
) (requestContext, error) {
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
		return requestContext{}, err
	}
	protectedNames := append(credentials.ProtectedNames(snapshot.Config), s.protectedNames...)
	if workingDirectory == "" {
		workingDirectory, err = os.Getwd()
		if err != nil {
			return requestContext{}, err
		}
	}
	if environment == nil {
		environment = s.environment()
	}
	environment = credentials.StripEnvironment(environment, protectedNames)
	resolver := s.build(ResolverOptions{
		WorkingDirectory: workingDirectory,
		Environment:      environment,
		ProtectedNames:   protectedNames,
	})
	return requestContext{
		resolver: resolver, config: snapshot,
		workingDirectory: workingDirectory,
		environment:      append([]string(nil), environment...),
	}, nil
}

func (s *PublicService) Acquire(
	ctx context.Context,
	request LeaseRequest,
) (Lease, error) {
	requestContext, err := s.requestContext(request.WorkingDirectory, request.Environment)
	if err != nil {
		return nil, err
	}
	request.WorkingDirectory = requestContext.workingDirectory
	request.Environment = requestContext.environment
	resolveRequest := ResolveRequest{Target: request.Snapshot.LogicalTarget}
	resolve := func(ctx context.Context) (RouteSnapshot, error) {
		return requestContext.resolver.Resolve(ctx, resolveRequest)
	}
	if err := s.initializeManager(requestContext.config.Config.SSH.IdleTimeout); err != nil {
		return nil, err
	}
	return s.leases.Acquire(ctx, request, resolve)
}

func (s *PublicService) initializeManager(idleTimeout time.Duration) error {
	s.managerMu.Lock()
	defer s.managerMu.Unlock()
	if s.closed {
		return connectionChanged()
	}
	if s.leases != nil {
		return nil
	}
	if s.managerErr != nil {
		return connectionFailed(s.managerErr)
	}
	if s.home == "" {
		return connectionFailed(errors.New("kwt home is required for SSH lifecycle"))
	}
	managerDirectory := filepath.Join(s.home, "runtime", "ssh-"+s.managerID)
	controlDirectory := filepath.Join(managerDirectory, "control")
	privateDirectory := filepath.Join(managerDirectory, "private")
	connectionOptions := openssh.DefaultConnectionOptions()
	persistenceTimeout := max(idleTimeout, minimumCrashPersistenceTimeout)
	connectionOptions.ControlPersistTimeout = persistenceTimeout
	persistent, err := s.newPersistent(controlDirectory, openssh.PersistentConfig{
		ConnectionOptions: &connectionOptions,
	})
	if err != nil {
		return connectionFailed(err)
	}
	s.leases = NewManager(ManagerOptions{
		Persistent:       persistent,
		PrivateDirectory: privateDirectory,
		Runner: func(request LeaseRequest, target ResolvedTarget) (openssh.RunSSH, error) {
			return newRunner(privateDirectory, request, target, runnerOptions{
				Executable: s.askpassExecutable,
			})
		},
		IdleTimeout:        idleTimeout,
		PersistenceTimeout: persistenceTimeout,
		OnEvent:            s.onEvent,
	})
	s.managerDir = managerDirectory
	return nil
}

func (s *PublicService) Close(ctx context.Context) error {
	s.managerMu.Lock()
	s.closed = true
	manager := s.leases
	managerDirectory := s.managerDir
	s.managerMu.Unlock()
	if manager == nil {
		return nil
	}
	if err := manager.Close(ctx); err != nil {
		return err
	}
	if managerDirectory == "" {
		return nil
	}
	return os.RemoveAll(managerDirectory)
}
