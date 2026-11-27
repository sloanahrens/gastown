package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reconcile"
)

// TestReconcilerPassKeepsObservations: an active pass keeps the observations it
// judged, dutyObservations returns them, a later pass replaces them, and a
// caller mutating the returned slice leaves the stored observations unchanged.
// Dropping the store in runReconciler turns this red (gt-m4ubr4).
func TestReconcilerPassKeepsObservations(t *testing.T) {
	t.Parallel()
	d := liveDaemon(t, time.Now())
	d.patrolConfig.Patrols.Reconciler.Enabled = true

	if got := d.dutyObservations(); len(got) != 0 {
		t.Fatalf("dutyObservations before any pass = %+v, want none", got)
	}

	d.runReconciler()

	got := d.dutyObservations()
	const want = "archive=ok backup=ok gc:gastown=ok gc:hq=ok town_state=ok"
	if sig := liveSignature(got); sig != want {
		t.Fatalf("dutyObservations = %q, want the pass's observations %q", sig, want)
	}

	// The accessor's copy: mutating it must not change what the pass kept.
	got[0] = reconcile.Observation{Duty: "forged", Verdict: reconcile.VerdictOverdue}
	if sig := liveSignature(d.dutyObservations()); sig != want {
		t.Errorf("dutyObservations after mutating a returned slice = %q, want %q", sig, want)
	}

	// The next pass replaces the kept observations: an unreadable gc state
	// makes that verdict unknown, and the accessor reports the new pass.
	statePath := maintenanceGCStatePath(d.config.TownRoot)
	if err := os.WriteFile(statePath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.runReconciler()
	const want2 = "archive=ok backup=ok gc=unknown! town_state=ok"
	if sig := liveSignature(d.dutyObservations()); sig != want2 {
		t.Errorf("dutyObservations after a second pass = %q, want %q", sig, want2)
	}
}
