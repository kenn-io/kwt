package kwt_test

import (
	"context"
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

func TestRootPackageExposesSSHResolutionService(t *testing.T) {
	service := kwt.NewSSHService(kwt.SSHServiceOptions{})
	if service == nil {
		t.Fatal("SSH resolution service is unavailable from the root package")
	}
	var _ interface {
		Resolve(context.Context, kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error)
	} = service
	if kwt.SSHProjectionPolicyV1 == "" {
		t.Fatal("SSH projection policy is unavailable from the root package")
	}
}
