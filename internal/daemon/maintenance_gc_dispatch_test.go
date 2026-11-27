package daemon

import (
	"testing"
	"time"
)

// A caller that already holds maintenanceGCRunning can dispatch a cycle
// without releasing it: dispatchMaintenanceGC runs the cycle, not taking the
// flag, and the dispatched func clears it when the cycle ends. startMaintenanceGC
// still refuses to dispatch while the flag is held, and leaves the holder's flag
// alone (gt-i09uqo).
func TestDispatchMaintenanceGCDispatchesWhileCallerHoldsFlag(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	withGCFakes(t, d)
	var dispatched int
	// Run the dispatched cycle inline so the test observes it finish, and count
	// it: the flag is cleared only by the cycle's own dispatched func.
	d.maint.dispatch = func(fn func()) {
		dispatched++
		fn()
	}

	d.maintenanceGCRunning.Store(true)
	d.dispatchMaintenanceGC([]string{"hq"}, gcTestNow.Add(time.Hour), nil, true)

	if dispatched != 1 {
		t.Fatalf("dispatchMaintenanceGC dispatched %d cycle(s) while the caller held the flag, want 1", dispatched)
	}
	if d.maintenanceGCRunning.Load() {
		t.Error("maintenanceGCRunning is still set after the dispatched cycle ended")
	}

	d.maintenanceGCRunning.Store(true)
	d.startMaintenanceGC([]string{"hq"}, gcTestNow.Add(time.Hour), nil, true)

	if dispatched != 1 {
		t.Errorf("startMaintenanceGC dispatched a cycle beside the running one: dispatches = %d, want still 1", dispatched)
	}
	if !d.maintenanceGCRunning.Load() {
		t.Error("startMaintenanceGC cleared a flag it found already set")
	}
}
