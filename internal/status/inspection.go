package status

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/service"
)

type InspectionRequest struct {
	Path               string `json:"path"`
	ExpectedRepository string `json:"expected_repository,omitempty"`
	ExpectedGeneration string `json:"expected_generation,omitempty"`
}

type WorktreeIdentity struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Generation string `json:"generation"`
}

type InspectionResult struct {
	Worktree   WorktreeIdentity `json:"worktree"`
	Changes    ChangeSet        `json:"changes"`
	ObservedAt time.Time        `json:"observed_at"`
}

type Inspector interface {
	Inspect(context.Context, InspectionRequest) (InspectionResult, error)
}

type InspectionServiceOptions struct {
	Inventory lifecycle.Inventory
}

type inspectionService struct {
	inventory        lifecycle.Inventory
	captureExpansion func() (lifecycle.ExpansionContext, error)
	readGeneration   func(context.Context, string, []string) (string, error)
	collectChanges   func(context.Context, string, []string) (ChangeSet, error)
	gitBudget        time.Duration
	now              func() time.Time
}

func NewInspectionService(options InspectionServiceOptions) Inspector {
	return &inspectionService{
		inventory:        options.Inventory,
		captureExpansion: lifecycle.CaptureExpansionContext,
		readGeneration: func(
			ctx context.Context,
			path string,
			protectedNames []string,
		) (string, error) {
			return git.NewForInventory(
				ctx,
				path,
				protectedNames,
			).ReadWorktreeGeneration(path)
		},
		collectChanges: CollectChanges,
		gitBudget:      collectChangesTimeout,
		now:            time.Now,
	}
}

func (s *inspectionService) Inspect(
	ctx context.Context,
	request InspectionRequest,
) (InspectionResult, error) {
	if !filepath.IsAbs(request.Path) {
		return InspectionResult{}, inspectionInvalid("worktree path must be absolute")
	}
	if request.ExpectedGeneration != "" {
		if err := git.ValidateWorktreeGeneration(request.ExpectedGeneration); err != nil {
			return InspectionResult{}, inspectionInvalid(
				"expected generation must be a 32-character hexadecimal value",
			)
		}
	}
	if request.ExpectedRepository != "" &&
		!validInspectionRepositoryIdentity(request.ExpectedRepository) {
		return InspectionResult{}, inspectionInvalid(
			"expected repository identity is invalid",
		)
	}
	if err := ctx.Err(); err != nil {
		return InspectionResult{}, err
	}
	if s.inventory == nil {
		return InspectionResult{}, inspectionFailure(nil)
	}
	expansion, err := s.captureExpansion()
	if err != nil {
		return InspectionResult{}, inspectionFailure(err)
	}
	inventory, err := s.inventory.Query(ctx, lifecycle.Request{
		View:             lifecycle.ViewRepository,
		WorkingDirectory: request.Path,
		Expansion:        expansion,
		RequireCurrent:   true,
		UntrustedConfig:  lifecycle.IgnoreUntrustedConfig,
	})
	if contextErr := ctx.Err(); contextErr != nil {
		return InspectionResult{}, contextErr
	}
	if err != nil {
		var typed *service.Error
		if errors.As(err, &typed) {
			return InspectionResult{}, err
		}
		return InspectionResult{}, inspectionFailure(err)
	}
	pathKey := utils.PathKey(request.Path)
	matches := make([]lifecycle.Entry, 0, 1)
	for _, entry := range inventory.Snapshot.Entries {
		if utils.PathKey(entry.Path) == pathKey {
			matches = append(matches, entry)
		}
	}
	switch len(matches) {
	case 0:
		return InspectionResult{}, service.NewError(
			service.NotFound,
			"no inventory worktree matches the exact path",
			false,
			nil,
			nil,
		)
	case 1:
	default:
		return InspectionResult{}, inspectionFailure(nil)
	}
	entry := matches[0]
	if entry.Path == "" ||
		!validInspectionRepositoryIdentity(entry.Repository.FullPath) ||
		git.ValidateWorktreeGeneration(entry.Generation) != nil {
		return InspectionResult{}, inspectionFailure(nil)
	}
	if request.ExpectedRepository != "" &&
		!lifecycle.EqualProjectIdentity(
			request.ExpectedRepository,
			entry.Repository.FullPath,
		) {
		return InspectionResult{}, inspectionRegistrationChanged(
			"worktree repository no longer matches the expected repository",
			nil,
		)
	}
	if request.ExpectedGeneration != "" &&
		request.ExpectedGeneration != entry.Generation {
		return InspectionResult{}, inspectionRegistrationChanged(
			"worktree generation no longer matches the expected generation",
			nil,
		)
	}
	if inventory.Snapshot.Config == nil {
		return InspectionResult{}, inspectionFailure(
			errors.New("inventory omitted effective configuration"),
		)
	}
	protectedNames := credentials.ProtectedNames(inventory.Snapshot.Config)
	gitContext, cancel := context.WithTimeout(ctx, s.gitBudget)
	defer cancel()
	before, err := s.readGeneration(gitContext, entry.Path, protectedNames)
	if err != nil {
		if cancellation := inspectionContextError(ctx, gitContext, err); cancellation != nil {
			return InspectionResult{}, cancellation
		}
		if inspectionRegistrationDidChange(err) {
			return InspectionResult{}, inspectionRegistrationChanged(
				"worktree registration changed after inventory",
				err,
			)
		}
		return InspectionResult{}, inspectionFailure(err)
	}
	if contextErr := inspectionExpiredContext(ctx, gitContext); contextErr != nil {
		return InspectionResult{}, contextErr
	}
	if before != entry.Generation {
		return InspectionResult{}, inspectionRegistrationChanged(
			"worktree generation changed after inventory",
			nil,
		)
	}
	changes, err := s.collectChanges(gitContext, entry.Path, protectedNames)
	if err != nil {
		if cancellation := inspectionContextError(ctx, gitContext, err); cancellation != nil {
			return InspectionResult{}, cancellation
		}
		after, generationErr := s.readGeneration(
			gitContext,
			entry.Path,
			protectedNames,
		)
		if generationErr != nil {
			if cancellation := inspectionContextError(
				ctx,
				gitContext,
				generationErr,
			); cancellation != nil {
				return InspectionResult{}, cancellation
			}
			if inspectionRegistrationDidChange(generationErr) {
				return InspectionResult{}, inspectionRegistrationChanged(
					"worktree registration changed during inspection",
					generationErr,
				)
			}
			return InspectionResult{}, inspectionFailure(err)
		}
		if contextErr := inspectionExpiredContext(ctx, gitContext); contextErr != nil {
			return InspectionResult{}, contextErr
		}
		if after != entry.Generation {
			return InspectionResult{}, inspectionRegistrationChanged(
				"worktree generation changed during inspection",
				nil,
			)
		}
		return InspectionResult{}, inspectionFailure(err)
	}
	if contextErr := inspectionExpiredContext(ctx, gitContext); contextErr != nil {
		return InspectionResult{}, contextErr
	}
	after, err := s.readGeneration(gitContext, entry.Path, protectedNames)
	if err != nil {
		if cancellation := inspectionContextError(ctx, gitContext, err); cancellation != nil {
			return InspectionResult{}, cancellation
		}
		if inspectionRegistrationDidChange(err) {
			return InspectionResult{}, inspectionRegistrationChanged(
				"worktree registration changed during inspection",
				err,
			)
		}
		return InspectionResult{}, inspectionFailure(err)
	}
	if contextErr := inspectionExpiredContext(ctx, gitContext); contextErr != nil {
		return InspectionResult{}, contextErr
	}
	if after != entry.Generation {
		return InspectionResult{}, inspectionRegistrationChanged(
			"worktree generation changed during inspection",
			nil,
		)
	}
	return InspectionResult{
		Worktree: WorktreeIdentity{
			Repository: entry.Repository.FullPath,
			Path:       entry.Path,
			Generation: entry.Generation,
		},
		Changes:    changes,
		ObservedAt: s.now().UTC(),
	}, nil
}

func inspectionContextError(
	parent context.Context,
	operation context.Context,
	err error,
) error {
	if cancellation := parent.Err(); cancellation != nil && errors.Is(err, cancellation) {
		return cancellation
	}
	if timeout := operation.Err(); timeout != nil && errors.Is(err, timeout) {
		return inspectionTimeout(err)
	}
	return nil
}

func inspectionExpiredContext(
	parent context.Context,
	operation context.Context,
) error {
	if cancellation := parent.Err(); cancellation != nil {
		return cancellation
	}
	if timeout := operation.Err(); timeout != nil {
		return inspectionTimeout(timeout)
	}
	return nil
}

func inspectionRegistrationDidChange(err error) bool {
	return errors.Is(err, git.ErrWorktreeNotFound) ||
		errors.Is(err, git.ErrWorktreeRepositoryMismatch) ||
		errors.Is(err, git.ErrWorktreeGenerationNotFound)
}

func validInspectionRepositoryIdentity(identity string) bool {
	// EqualProjectIdentity validates each operand before folding and comparing;
	// self-comparison therefore provides validation without a second policy.
	return lifecycle.EqualProjectIdentity(identity, identity)
}

func inspectionInvalid(message string) error {
	return service.NewError(service.InvalidRequest, message, false, nil, nil)
}

func inspectionFailure(cause error) error {
	message := "worktree inspection failed"
	if errors.Is(cause, git.ErrStdoutLimitExceeded) {
		message = "worktree change list is too large to inspect"
	}
	return service.NewError(
		service.InspectionFailed,
		message,
		false,
		nil,
		cause,
	)
}

func inspectionTimeout(cause error) error {
	return service.NewError(
		service.InspectionFailed,
		"worktree inspection timed out",
		true,
		nil,
		cause,
	)
}

func inspectionRegistrationChanged(message string, cause error) error {
	return service.NewError(
		service.RegistrationChanged,
		message,
		true,
		nil,
		cause,
	)
}
