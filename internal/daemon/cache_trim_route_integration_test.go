//go:build integration

package daemon

import "testing"

// TestIntegrationNewRoutesCacheTrimToTheClaimJob drives New itself: the daemon
// production builds trims through the claim Job, not the old listing route.
func TestIntegrationNewRoutesCacheTrimToTheClaimJob(t *testing.T) {
	d := newDaemonForEnvWiring(t, "")
	if !d.cacheTrimClaimJobRoute {
		t.Fatal("the daemon New builds has cacheTrimClaimJobRoute false; production would trim through the old listing route")
	}
}
