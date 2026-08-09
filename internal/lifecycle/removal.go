package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/service"
)

type RemovalRequest struct {
	RepositoryPath     string `json:"repository_path"`
	Path               string `json:"path"`
	ExpectedGeneration string `json:"expected_generation"`
	Force              bool   `json:"force,omitempty"`
	DeleteBranch       bool   `json:"delete_branch,omitempty"`
	ForceDeleteBranch  bool   `json:"force_delete_branch,omitempty"`
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
	Home string
}

type removalService struct {
	home string
}

func NewRemovalService(options RemovalServiceOptions) Remover {
	return &removalService{home: options.Home}
}

func (s *removalService) Remove(
	ctx context.Context,
	request RemovalRequest,
) (RemovalResult, error) {
	result := RemovalResult{Path: request.Path}
	if !filepath.IsAbs(request.RepositoryPath) {
		return result, removalInvalid("repository path must be absolute")
	}
	if !filepath.IsAbs(request.Path) {
		return result, removalInvalid("worktree path must be absolute")
	}
	if err := git.ValidateWorktreeGeneration(request.ExpectedGeneration); err != nil {
		return result, removalInvalid("expected generation must be a 32-character hexadecimal value")
	}
	if err := ctx.Err(); err != nil {
		return result, classifyRemovalError(err, result)
	}

	repository := git.NewForInventory(ctx, request.RepositoryPath, nil)
	root, err := repository.GetMainRepositoryPath()
	if err != nil {
		return result, classifyRemovalError(fmt.Errorf("resolve main repository: %w", err), result)
	}
	reg, err := registry.NewAt(s.home)
	if err != nil {
		return result, classifyRemovalError(err, result)
	}
	record, registered := reg.Get(request.Path)
	var mutationErr error
	committed, err := reg.RemoveIfMatchAfter(request.Path, record, func() error {
		transaction, transactionErr := git.NewForInventory(ctx, root, nil).RemoveWorktreeTransaction(
			request.Path,
			request.ExpectedGeneration,
			request.Force,
			request.DeleteBranch,
			request.ForceDeleteBranch,
		)
		result.Path = transaction.Path
		result.Branch = transaction.Branch
		result.WorktreeRemoved = transaction.WorktreeRemoved
		result.BranchDeleted = transaction.BranchDeleted
		mutationErr = transactionErr
		if transaction.WorktreeRemoved {
			return nil
		}
		return transactionErr
	})
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
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return service.NewError(service.Busy, "worktree removal canceled", true, details, err)
	}
	return service.NewError(service.Internal, boundedDiagnostic(err), false, details, err)
}

func requestSafePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return ""
}
