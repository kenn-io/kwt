package kwt_test

import (
	"context"
	"io"
	"testing"

	kwt "go.kenn.io/kwt"
)

func TestRootPackageExposesWorktreeServices(t *testing.T) {
	if kwt.NewInventoryService(kwt.InventoryServiceOptions{}) == nil {
		t.Fatal("inventory service is unavailable from the root package")
	}
	if kwt.NewRemovalService(kwt.RemovalServiceOptions{}) == nil {
		t.Fatal("removal service is unavailable from the root package")
	}
}

func TestRootPackageExposesInspectionService(t *testing.T) {
	inspector := kwt.NewInspectionService(kwt.InspectionServiceOptions{})
	if inspector == nil {
		t.Fatal("inspection service is unavailable from the root package")
	}
	var _ = kwt.InspectionRequest{}
	var _ = kwt.InspectionResult{
		Worktree: kwt.WorktreeIdentity{},
		Changes: kwt.ChangeSet{
			State: kwt.ChangeStateClean,
			Files: []kwt.FileChange{{
				Index: kwt.FileStateAdded,
			}},
		},
	}
}

func TestRootPackageExposesSSHAskpassDispatch(t *testing.T) {
	exitCode, handled := kwt.RunSSHAskpassHelper(
		[]string{"host-application"},
		nil,
		io.Discard,
	)
	if handled || exitCode != 0 {
		t.Fatal("ordinary host process invocation was treated as SSH askpass")
	}
}

func TestRootPackageExposesSSHResolutionService(t *testing.T) {
	service := kwt.NewSSHService(kwt.SSHServiceOptions{})
	if service == nil {
		t.Fatal("SSH resolution service is unavailable from the root package")
	}
	var _ interface {
		Resolve(context.Context, kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error)
		Acquire(context.Context, kwt.SSHLeaseRequest) (kwt.SSHLease, error)
	} = service
	if kwt.SSHProjectionPolicyV1 == "" {
		t.Fatal("SSH projection policy is unavailable from the root package")
	}
	requireLeaseMode := func(kwt.SSHLeaseMode) {}
	requireLeaseMode(kwt.SSHLeaseModeMultiplexed)
	if kwt.SSHEventStateConnected == "" {
		t.Fatal("SSH event states are unavailable from the root package")
	}
}
