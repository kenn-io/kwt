package cmd

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func commandFlagChanged(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return false
	}
	flag := cmd.Flags().Lookup(name)
	return flag != nil && flag.Changed
}

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
	expectedIdentities ...string,
) (*guardedProjectOperation, error) {
	guard, err := observeGuardedProjectOperation(ctx, home, mainPath, expansion)
	if err != nil {
		return nil, err
	}
	if guard.claim == nil || !projectClaimHasExpectedIdentity(
		guard.claim, expectedIdentities,
	) {
		return nil, service.NewError(
			service.RegistrationChanged,
			"the project registration changed before the operation began",
			true, nil, nil,
		)
	}
	guard.required = true
	return guard, nil
}

func observeExpectedGuardedProjectOperation(
	ctx context.Context,
	home string,
	mainPath string,
	expansion kwt.ExpansionContext,
	expectedIdentity string,
	expectedRegistration string,
) (*guardedProjectOperation, error) {
	if !config.ValidProjectRegistrationFingerprint(expectedRegistration) {
		return nil, service.NewError(
			service.InvalidRequest,
			"expected project registration fingerprint is invalid",
			false, nil, nil,
		)
	}
	guard, err := observeRequiredGuardedProjectOperation(
		ctx, home, mainPath, expansion, expectedIdentity,
	)
	if err != nil {
		return nil, err
	}
	actualRegistration, err := guard.claim.Registration.Fingerprint()
	if err != nil || actualRegistration != expectedRegistration {
		return nil, service.NewError(
			service.RegistrationChanged,
			"the project registration changed before the operation began",
			true, nil, err,
		)
	}
	return guard, nil
}

func projectClaimHasExpectedIdentity(
	claim *lifecycle.ProjectClaim,
	expected []string,
) bool {
	if claim == nil || len(expected) == 0 {
		return false
	}
	for _, identity := range expected {
		if lifecycle.EqualProjectIdentity(claim.Identity, identity) {
			return true
		}
	}
	return false
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
