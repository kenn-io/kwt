package cmd

import (
	"context"
	"errors"

	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/lifecycle"
)

type guardedProjectOperation struct {
	home  string
	claim *lifecycle.ProjectClaim
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
	release, err := lifecycle.AcquireProjectClaim(ctx, g.home, g.claim)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, release())
	}()
	return mutation()
}
