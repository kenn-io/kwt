package worktree

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/pkg/models"
	shared "go.kenn.io/kwt/worktree"
)

// CreateOptions selects application naming, source trust, and setup policy.
// Source names an already fetched remote branch; an empty source with
// NewBranch=false attaches an existing local branch.
type CreateOptions struct {
	Branch, Path, Source                    string
	NewBranch, RequireGeneration, SkipSetup bool
}

// Create keeps naming, registry provenance, and warning-only setup in kwt.
// Git acquisition and cleanup authority belong to the shared lifecycle.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (shared.CreateResult, error) {
	path, err := m.preparePath(opts.Path, opts.Branch, nil)
	if err != nil {
		return shared.CreateResult{}, err
	}
	opts.Path = path
	if opts.Source != "" || !opts.NewBranch {
		return m.addUnreviewedSource(ctx, path, opts.Branch, func() (shared.CreateResult, error) { return m.createGit(ctx, opts) })
	}
	result, err := m.createGit(ctx, opts)
	if err != nil {
		return result, err
	}
	if !opts.SkipSetup {
		m.runPostWorktreeSetup(opts.Branch, path)
	}
	return result, nil
}

func createShared(ctx context.Context, g *git.Git, cfg *models.Config, opts CreateOptions) (result shared.CreateResult, err error) {
	isolated := opts.Source != "" || !opts.NewBranch
	var protected []string
	var config []gitcmd.Config
	if isolated {
		protected = credentials.ProtectedNames(cfg)
	}
	// This is command-scoped, matching the old explicit --track behavior even
	// when the user's default disables automatic remote tracking.
	if opts.Source != "" {
		config = []gitcmd.Config{{Key: "branch.autoSetupMerge", Value: "true"}}
	}
	execution, err := g.WorktreeExecution(protected, config)
	if err != nil {
		return result, err
	}
	repo, err := git.OpenWorktrees(ctx, execution)
	if err != nil {
		return result, err
	}
	req := shared.CreateRequest{Git: managed.CreateWorktreeOptions{Path: opts.Path, Branch: opts.Branch, Mode: managed.CheckoutNewBranch}, Identity: shared.IdentityPolicy{FileName: "kwt-generation", Generate: true}}
	if !isolated {
		req.Git.BaseRef, err = defaultWorktreeBase(ctx, repo, execution)
		if err != nil {
			return result, err
		}
		req.Serialization = shared.CreateHookReentrant
		result, err = repo.Create(ctx, req)
		if errors.Is(err, shared.ErrIdentityUnavailable) && !opts.RequireGeneration && ctx.Err() == nil {
			return result, nil
		}
		return result, err
	}
	req.Git.Checkout = managed.CheckoutIsolated
	req.Git.Mode = managed.CheckoutExistingBranch
	err = repo.WithLock(ctx, func(scope *shared.Scope) error {
		if opts.Source != "" {
			reused, e := reusableTrackingBranch(ctx, scope, opts.Branch, opts.Source)
			if e != nil {
				return e
			}
			if !reused {
				req.Git.Mode = managed.CheckoutNewBranch
				req.Git.BaseRef = opts.Source
			}
		}
		var createErr error
		result, createErr = scope.Create(ctx, req)
		if createErr == nil || errors.Is(createErr, shared.ErrIdentityUnavailable) || (result.Path == "" && result.OwnedBranch == "") {
			return createErr
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, cleanupErr := scope.Rollback(cleanupCtx, result, managed.RollbackFreshOwned)
		if cleanupErr != nil {
			return errors.Join(createErr, fmt.Errorf("failed to remove incomplete worktree: %w", cleanupErr))
		}
		result = shared.CreateResult{}
		return createErr
	})
	return result, err
}

func defaultWorktreeBase(ctx context.Context, repo *shared.Repository, execution shared.RepositoryOptions) (string, error) {
	const remoteRef = "refs/kwt/origin/default"
	_, remoteErr := execution.Runner.Output(ctx, execution.Path, "fetch", "origin", "+HEAD:"+remoteRef)
	exists := func(ref string) bool {
		_, err := execution.Runner.Output(ctx, execution.Path, "show-ref", "--verify", "--quiet", ref)
		return err == nil
	}
	if remoteErr == nil && exists(remoteRef) {
		return remoteRef, nil
	}
	for _, branch := range []string{"main", "master"} {
		ref := "refs/heads/" + branch
		if exists(ref) {
			return ref, nil
		}
	}
	inventory, err := repo.Inspect(ctx)
	if err == nil {
		for _, entry := range inventory.Entries {
			if entry.IsMain && entry.Branch != "" {
				ref := "refs/heads/" + entry.Branch
				if exists(ref) {
					return ref, nil
				}
			}
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", fmt.Errorf("could not resolve default worktree base: remote default unavailable (%v); no local main, master, or primary worktree branch", remoteErr)
}

func reusableTrackingBranch(ctx context.Context, scope *shared.Scope, branch, source string) (bool, error) {
	localRef := "refs/heads/" + branch
	if !strings.HasPrefix(source, "refs/") {
		source = "refs/remotes/" + source
	}
	output, err := scope.RunGit(ctx, "", "for-each-ref", "--format=%(refname)%00%(objectname)%00%(upstream)", "--", localRef, source)
	if err != nil {
		return false, fmt.Errorf("inspect local tracking branch: %w", err)
	}
	var localOID, sourceOID, upstream string
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 {
			return false, errors.New("inspect local tracking branch: invalid response")
		}
		switch parts[0] {
		case localRef:
			localOID, upstream = parts[1], parts[2]
		case source:
			sourceOID = parts[1]
		}
	}
	if localOID == "" {
		return false, nil
	}
	if upstream != source {
		return false, fmt.Errorf("local branch %s already exists with a different upstream", branch)
	}
	if sourceOID == "" || localOID != sourceOID {
		return false, fmt.Errorf("local branch %s points to a different commit than %s", branch, source)
	}
	return true, nil
}
