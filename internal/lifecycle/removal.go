package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/service"
)

type RemovalSessionCondition = tmux.RemovalSessionCondition
type RemovalSessionConditionError = tmux.RemovalSessionConditionError

type RemovalRequest struct {
	RepositoryPath     string                   `json:"repository_path"`
	Path               string                   `json:"path"`
	ExpectedGeneration string                   `json:"expected_generation"`
	ExpectedBranch     string                   `json:"expected_branch,omitempty"`
	ExpectedHead       string                   `json:"expected_head,omitempty"`
	Expansion          ExpansionContext         `json:"expansion,omitempty"`
	Force              bool                     `json:"force,omitempty"`
	DeleteBranch       bool                     `json:"delete_branch,omitempty"`
	ForceDeleteBranch  bool                     `json:"force_delete_branch,omitempty"`
	Session            *RemovalSessionCondition `json:"session,omitempty"`
}

type RemovalResult struct {
	Path                 string `json:"path"`
	Branch               string `json:"branch,omitempty"`
	WorktreeRemoved      bool   `json:"worktree_removed"`
	BranchDeleted        bool   `json:"branch_deleted,omitempty"`
	RegistryUnregistered bool   `json:"registry_unregistered,omitempty"`
}

type Remover interface {
	Remove(context.Context, RemovalRequest) (RemovalResult, error)
}

type RemovalServiceOptions struct {
	Home         string
	SessionGuard tmux.RemovalSessionGuard
	// ProcessGuard replaces the default live-process check. Embedders that set
	// it are responsible for enforcing their own worktree-use policy.
	ProcessGuard func(context.Context, string) error
}

type removalService struct {
	home         string
	sessionGuard tmux.RemovalSessionGuard
	processGuard func(context.Context, string) error
}

var newRemovalInventoryGit = git.NewForInventory

func NewRemovalService(options RemovalServiceOptions) Remover {
	guard := options.SessionGuard
	if guard == nil {
		guard = tmux.NewRemovalSessionGuard("")
	}
	processGuard := options.ProcessGuard
	if processGuard == nil {
		processGuard = rejectProcessesUsingWorktree
	}
	return &removalService{
		home:         options.Home,
		sessionGuard: guard,
		processGuard: processGuard,
	}
}

func (s *removalService) Remove(
	ctx context.Context,
	request RemovalRequest,
) (result RemovalResult, resultErr error) {
	result = RemovalResult{Path: request.Path}
	if !filepath.IsAbs(request.RepositoryPath) {
		return result, removalInvalid("repository path must be absolute")
	}
	if !filepath.IsAbs(request.Path) {
		return result, removalInvalid("worktree path must be absolute")
	}
	if err := git.ValidateWorktreeGeneration(request.ExpectedGeneration); err != nil {
		return result, removalInvalid("expected generation must be a 32-character hexadecimal value")
	}
	if request.Session != nil {
		if err := request.Expansion.validate(); err != nil {
			return result, removalInvalid(err.Error())
		}
	}
	if err := ctx.Err(); err != nil {
		return result, classifyRemovalError(err, result)
	}
	var protectedNames []string
	if request.Session != nil {
		configSnapshot, err := config.LoadGlobalSnapshotAtWithExpansion(
			s.home,
			request.Expansion.expandPath,
		)
		if err != nil {
			return result, classifyRemovalError(
				fmt.Errorf("reload removal credential policy: %w", err),
				result,
			)
		}
		protectedNames = credentials.ProtectedNames(configSnapshot.Config)
	}

	repository := newRemovalInventoryGit(ctx, request.RepositoryPath, protectedNames)
	root, err := repository.GetMainRepositoryPath()
	if err != nil {
		return result, classifyRemovalError(fmt.Errorf("resolve main repository: %w", err), result)
	}
	var (
		projectClaim    *ProjectClaim
		protectedTarget *removalProtectedSessionTarget
	)
	if request.Session != nil {
		var releaseProject func() error
		projectClaim, releaseProject, err = acquireRemovalProjectFence(
			ctx, s.home, root, request.Expansion,
		)
		if err != nil {
			return result, classifyRemovalError(err, result)
		}
		defer func() {
			if releaseErr := releaseProject(); releaseErr != nil {
				resultErr = errors.Join(
					resultErr,
					classifyRemovalError(
						fmt.Errorf("release project lifecycle lock: %w", releaseErr),
						result,
					),
				)
			}
		}()
		protectedTarget, err = observeRemovalProtectedSessionTarget(
			ctx, s.home, request.Path, request.ExpectedGeneration, projectClaim,
		)
		if err != nil {
			return result, classifyRemovalError(err, result)
		}
	}

	reg, err := registry.NewAt(s.home)
	if err != nil {
		return result, classifyRemovalError(err, result)
	}
	release, acquired, err := reg.AcquireCreation(request.Path)
	if err != nil {
		return result, classifyRemovalError(err, result)
	}
	if !acquired {
		return result, service.NewError(
			service.Conflict,
			fmt.Sprintf("worktree creation is in progress for %s", request.Path),
			true,
			map[string]any{"path": request.Path, "reason": "creation_in_progress"},
			nil,
		)
	}
	defer func() {
		if err := release(); err != nil {
			releaseErr := classifyRemovalError(
				fmt.Errorf("release worktree creation lock: %w", err),
				result,
			)
			if resultErr == nil {
				resultErr = releaseErr
			} else {
				resultErr = errors.Join(resultErr, releaseErr)
			}
		}
	}()

	record, registered := reg.Get(request.Path)
	var mutationErr error
	transaction, committed, err := newRemovalInventoryGit(
		ctx,
		root,
		protectedNames,
	).RemoveWorktreeTransactionAfterClaim(
		request.Path,
		request.ExpectedGeneration,
		request.ExpectedBranch,
		request.ExpectedHead,
		request.Force,
		request.DeleteBranch,
		request.ForceDeleteBranch,
		request.Session != nil,
		func(preflight func() error, remove func() error) (bool, error) {
			return reg.RemoveIfMatchAfter(request.Path, record, func() error {
				if request.Session != nil {
					sessionCondition := *request.Session
					// compat(kag1): default-server adoption
					if sessionCondition.SocketDirectory == "" {
						sessionCondition.SocketDirectory = removalSocketDirectory(
							request.Expansion,
						)
					}
					sessionCondition.WorkspacePath = request.Path
					sessionCondition.WorkspaceGeneration = request.ExpectedGeneration
					sessionCondition.ProtectedSocketTopology = protectedTarget != nil
					sessionCondition.ProtectedNames = append([]string(nil), protectedNames...)
					if err := validateCurrentRemovalSessionTarget(
						ctx,
						request.Path,
						projectClaim,
						protectedTarget,
						request.Expansion,
						sessionCondition,
					); err != nil {
						return err
					}
					if err := quiescePreflightAndTerminate(
						ctx, s.sessionGuard, sessionCondition, func() error {
							if err := preflight(); err != nil {
								return err
							}
							if request.Force {
								return nil
							}
							return s.processGuard(ctx, request.Path)
						},
					); err != nil {
						return err
					}
				} else if !request.Force {
					if err := s.processGuard(ctx, request.Path); err != nil {
						return err
					}
				}
				mutationErr = remove()
				if git.WorktreeWasRemoved(mutationErr) {
					return nil
				}
				return mutationErr
			})
		},
	)
	result.Path = transaction.Path
	result.Branch = transaction.Branch
	result.WorktreeRemoved = transaction.WorktreeRemoved
	result.BranchDeleted = transaction.BranchDeleted
	if err != nil {
		return result, classifyRemovalError(err, result)
	}
	if !committed {
		return result, service.NewError(
			service.Conflict,
			"worktree registry changed before removal",
			true,
			map[string]any{"path": request.Path},
			nil,
		)
	}
	result.RegistryUnregistered = registered
	if mutationErr != nil {
		return result, classifyRemovalError(mutationErr, result)
	}
	return result, nil
}

func quiescePreflightAndTerminate(
	ctx context.Context,
	guard tmux.RemovalSessionGuard,
	condition RemovalSessionCondition,
	preflight func() error,
) (resultErr error) {
	lease, err := guard.Quiesce(ctx, condition)
	if err != nil {
		return err
	}
	terminated := false
	defer func() {
		if !terminated {
			resultErr = errors.Join(resultErr, lease.Resume())
		}
	}()
	if err := preflight(); err != nil {
		return err
	}
	if err := lease.Terminate(ctx); err != nil {
		return err
	}
	terminated = true
	return nil
}

func acquireRemovalProjectFence(
	ctx context.Context,
	home string,
	root string,
	expansion ExpansionContext,
) (*ProjectClaim, func() error, error) {
	claim, err := ObserveProjectClaim(ctx, home, root, expansion)
	if err != nil {
		return nil, nil, err
	}
	release, err := AcquireRequiredProjectClaim(ctx, home, claim)
	if err != nil {
		return nil, nil, err
	}
	return claim, release, nil
}

func removalInvalid(message string) error {
	return service.NewError(service.InvalidRequest, message, false, nil, nil)
}

func classifyRemovalError(err error, result RemovalResult) error {
	if err == nil {
		return nil
	}
	var typed *service.Error
	if errors.As(err, &typed) {
		return err
	}
	details := map[string]any{
		"path":                  requestSafePath(result.Path),
		"branch":                result.Branch,
		"worktree_removed":      result.WorktreeRemoved,
		"branch_deleted":        result.BranchDeleted,
		"registry_unregistered": result.RegistryUnregistered,
	}
	var condition *git.ConditionError
	if errors.As(err, &condition) {
		details["reason"] = string(condition.Reason)
		message := condition.Error()
		if condition.Reason == git.ReasonGenerationChanged {
			message = fmt.Sprintf("worktree generation changed for %s", condition.Path)
		}
		return service.NewError(service.Conflict, message, true, details, err)
	}
	var sessionCondition *RemovalSessionConditionError
	if errors.As(err, &sessionCondition) {
		details["reason"] = sessionCondition.Error()
		return service.NewError(service.Conflict, sessionCondition.Error(), true, details, err)
	}
	var processCondition *worktreeProcessConditionError
	if errors.As(err, &processCondition) {
		details["reason"] = "process_working_directory_live"
		return service.NewError(service.Conflict, processCondition.Error(), true, details, err)
	}
	var processInspection *worktreeProcessInspectionError
	if errors.As(err, &processInspection) {
		details["reason"] = "process_working_directory_indeterminate"
		return service.NewError(service.Conflict, processInspection.Error(), true, details, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return service.NewError(service.Busy, "worktree removal canceled", true, details, err)
	}
	if git.WorktreeWasRemoved(err) || git.IsWorktreeRemovalCommandError(err) {
		return service.NewError(service.RemovalFailed, boundedDiagnostic(err), false, details, err)
	}
	return service.NewError(service.Internal, "internal failure", false, details, err)
}

func requestSafePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return ""
}
