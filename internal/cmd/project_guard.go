package cmd

import (
	"context"
	"errors"

	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/pkg/models"
)

type guardedProjectOperation struct {
	home     string
	claim    *lifecycle.ProjectClaim
	required bool
}

func observeRequiredGuardedProjectOperation(
	ctx context.Context,
	home string,
	mainPath string,
	expansion kwt.ExpansionContext,
) (*guardedProjectOperation, error) {
	guard, err := observeGuardedProjectOperation(ctx, home, mainPath, expansion)
	if err != nil {
		return nil, err
	}
	guard.required = true
	return guard, nil
}

var beforeProjectGuardAcquire = func() {}

func observeGuardedProjectOperation(
	ctx context.Context,
	home string,
	mainPath string,
	expansion kwt.ExpansionContext,
) (*guardedProjectOperation, error) {
	claim, err := lifecycle.ObserveProjectClaim(
		ctx, home, mainPath, expansion,
	)
	if err != nil {
		return nil, err
	}
	return &guardedProjectOperation{home: home, claim: claim}, nil
}

func (g *guardedProjectOperation) run(
	ctx context.Context,
	mutation func() error,
) (err error) {
	beforeProjectGuardAcquire()
	acquire := lifecycle.AcquireProjectClaim
	if g.required {
		acquire = lifecycle.AcquireRequiredProjectClaim
	}
	release, err := acquire(ctx, g.home, g.claim)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, release())
	}()
	return mutation()
}

func registerProjectWithLifecycle(
	ctx context.Context,
	project models.Project,
) error {
	home, err := config.CanonicalHome()
	if err != nil {
		return err
	}
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return err
	}
	return lifecycle.TransitionProjectRegistration(
		ctx,
		home,
		expansion,
		project,
		func() error { return config.RegisterProject(project) },
	)
}
