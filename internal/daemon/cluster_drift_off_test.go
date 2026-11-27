package daemon

import (
	"context"
	"testing"
)

// TestClusterDriftSettingOffMakesNoCompareCall is the bead's box: a daemon as
// built over a town whose settings carry no cluster_drift block runs with the
// patrol off, so the sources' collector raises no item and never calls the
// compare that sits on the daemon (gt-dawn7d). A compare that was called would
// raise cluster.drift, so a test that ignored the setting would see both.
func TestClusterDriftSettingOffMakesNoCompareCall(t *testing.T) {
	t.Parallel()
	d, _, _ := patrolReloadTown(t)
	calls := 0
	d.clusterDriftCompare = func(context.Context) (clusterDriftResult, error) {
		calls++
		return clusterDriftResult{Differing: []string{"Deployment/gastown-daemon: spec.replicas 2 -> 3"}}, nil
	}

	items := clusterDriftHeartbeat(t, d)
	if calls != 0 {
		t.Errorf("the collector called the compare %d times with cluster_drift off, want none", calls)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v with cluster_drift off, want nothing", items)
	}
}
