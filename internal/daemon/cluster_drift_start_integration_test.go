//go:build integration

package daemon

import (
	"testing"
	"time"
)

// TestIntegrationNewRecordsTheDriftPatrolStart drives New itself and checks the
// daemon recorded its start between the call's bounds, so the drift patrol's
// start delay runs from when the daemon started. Deleting the New assignment
// leaves the start zero and turns it red (gt-myyr6e).
func TestIntegrationNewRecordsTheDriftPatrolStart(t *testing.T) {
	before := time.Now()
	d := newDaemonForEnvWiring(t, "")
	after := time.Now()

	started := d.clusterDriftCache.started
	if started.Before(before) || started.After(after) {
		t.Fatalf("clusterDriftCache.started = %v, want between %v and %v", started, before, after)
	}
}
