package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/lifecycle"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func runWorktreeSessionEstablishment(
	ctx context.Context,
	worktreePath string,
	expectedGeneration string,
	protectedNames []string,
	establish func(string) error,
) (string, error) {
	mainPath, err := git.NewWithContext(ctx, worktreePath).GetMainRepositoryPath()
	if err != nil {
		return "", fmt.Errorf("resolve selected repository root: %w", err)
	}
	home, err := config.CanonicalHome()
	if err != nil {
		return "", err
	}
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return "", err
	}
	guard, err := observeGuardedProjectOperation(ctx, home, mainPath, expansion)
	if err != nil {
		return "", err
	}
	projects := []models.Project(nil)
	if guard.claim != nil {
		projects = []models.Project{guard.claim.Registration.Effective}
	}
	var sessionName string
	err = guard.run(ctx, func() error {
		var establishErr error
		sessionName, establishErr = withCurrentWorktreeSession(
			ctx,
			mainPath,
			worktreePath,
			expectedGeneration,
			projects,
			protectedNames,
			establish,
		)
		return establishErr
	})
	return sessionName, err
}

func withCurrentWorktreeSession(
	ctx context.Context,
	mainPath string,
	worktreePath string,
	expectedGeneration string,
	projects []models.Project,
	protectedNames []string,
	establish func(string) error,
) (string, error) {
	var sessionName string
	err := git.NewWithContext(ctx, mainPath).WithWorktreeGeneration(
		worktreePath,
		expectedGeneration,
		func() error {
			var err error
			sessionName, _, err = lifecycle.ResolveCurrentWorktreeSessionIdentity(
				ctx,
				worktreePath,
				projects,
				protectedNames,
			)
			if err != nil {
				return err
			}
			return establish(sessionName)
		},
	)
	return sessionName, err
}

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
	if guard.claim == nil || !guard.claim.Registered || !projectClaimHasExpectedIdentity(
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
	_, err := registerProjectIdentityWithLifecycle(ctx, project)
	return err
}

func registerProjectIdentityWithLifecycle(
	ctx context.Context,
	project models.Project,
) (kwt.Project, error) {
	home, err := config.CanonicalHome()
	if err != nil {
		return kwt.Project{}, err
	}
	expansion, err := kwt.CaptureExpansionContext()
	if err != nil {
		return kwt.Project{}, err
	}
	canonicalIdentity, err := lifecycle.ResolveProspectiveProjectIdentity(
		ctx, home, expansion, project,
	)
	if err != nil {
		return kwt.Project{}, err
	}
	return lifecycle.TransitionProjectRegistrationWithIdentity(
		ctx,
		home,
		expansion,
		project,
		func() (kwt.Project, error) {
			registered, registerErr := config.RegisterProjectWithIdentity(project)
			if registerErr != nil {
				return kwt.Project{}, registerErr
			}
			return kwt.Project{
				Repository:              canonicalIdentity,
				Name:                    registered.Project.Name,
				Path:                    registered.Project.Path,
				LastTouched:             registered.Project.LastTouched,
				RegistrationFingerprint: registered.Fingerprint,
			}, nil
		},
	)
}
