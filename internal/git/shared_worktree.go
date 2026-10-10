package git

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	gitcmd "go.kenn.io/kit/git/cmd"
	"go.kenn.io/kwt/internal/credentials"
	shared "go.kenn.io/kwt/worktree"
)

var workspaceCoordinator = sync.OnceValues(func() (*shared.Coordinator, error) {
	return shared.NewCoordinator(shared.LockPolicy{
		FileName: "kwt-worktree.lock", ResolveCommonDirSymlinks: true,
		CreationLockName: "kwt-worktree-create.lock", CreationPathName: "kwt-worktree-create.path",
	})
})

// OpenWorktrees shares kwt's lock coordinator across application entry points.
// The caller supplies execution policy; the repository retains no context.
func OpenWorktrees(ctx context.Context, opts shared.RepositoryOptions) (*shared.Repository, error) {
	coordinator, err := workspaceCoordinator()
	if err != nil {
		return nil, err
	}
	return coordinator.Open(ctx, opts)
}

// WorktreeExecution translates kwt's credentials and process settings for
// application preparation that runs before acquiring a creation reservation.
func (g *Git) WorktreeExecution(protectedNames []string, config []gitcmd.Config) (shared.RepositoryOptions, error) {
	var err error
	path := g.workDir
	if path == "" {
		path, err = os.Getwd()
		if err != nil {
			return shared.RepositoryOptions{}, err
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return shared.RepositoryOptions{}, err
	}
	environment := g.environment
	if environment == nil {
		environment = os.Environ()
	}
	if protectedNames != nil {
		environment = credentials.StripEnvironment(environment, protectedNames)
	}
	runner := gitcmd.Runner{
		Env: environment, Config: config, TerminalPrompt: true,
		WaitDelay: commandWaitDelay, AcceptSuccessfulWaitDelay: true,
		DisableSafeDirectoryForward: true,
	}
	return shared.RepositoryOptions{Path: path, Runner: runner}, nil
}

// WorktreeRepository opens shared lifecycle operations with this application's
// environment and credential policy. Each operation still takes its own context.
func (g *Git) WorktreeRepository(ctx context.Context, protectedNames []string) (*shared.Repository, error) {
	opts, err := g.WorktreeExecution(protectedNames, nil)
	if err != nil {
		return nil, err
	}
	return OpenWorktrees(ctx, opts)
}
