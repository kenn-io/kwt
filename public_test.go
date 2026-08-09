package kwt_test

import (
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
