package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
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

// TestDutyStatePassWritesIt: one pass leaves the duty state record holding that
// pass's observations in order, each with its family's limit in seconds (gc:<db>
// reads the gc limit). Removing the call from runReconciler turns this red
// (gt-pu7tka).
func TestDutyStatePassWritesIt(t *testing.T) {
	t.Parallel()
	d := liveDaemon(t, time.Now())
	d.patrolConfig.Patrols.Reconciler.Enabled = true

	d.runReconciler()

	state, err := ReadDutyState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("ReadDutyState after a pass = %v, want the record the pass wrote", err)
	}
	// backup, archive, gc:gastown, gc:hq, town_state: the defaults are 24h,
	// 24h, 164h and 24h.
	const want = "backup=ok/86400 archive=ok/86400 gc:gastown=ok/590400 gc:hq=ok/590400 town_state=ok/86400"
	if sig := dutyRecordSignature(state.Duties); sig != want {
		t.Errorf("duty state records = %q, want %q", sig, want)
	}
}

// TestDutyStatePassWriteFails: with the record's path occupied by a directory
// the write fails. The pass logs that failure as one line after its own, and
// still records its run.
func TestDutyStatePassWriteFails(t *testing.T) {
	t.Parallel()
	d := liveDaemon(t, time.Now())
	d.patrolConfig.Patrols.Reconciler.Enabled = true
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	occupied := dutyStatePath(d.config.TownRoot)
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	d.runReconciler()

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 || lines[0] != "reconciler: pass (shadow)" ||
		!strings.HasPrefix(lines[1], "reconciler: write duty state: ") {
		t.Errorf("log = %q, want the pass line then one write-failed line", logs.String())
	}
	if last := d.reconcilerRun.last(); last.Before(before) {
		t.Errorf("last run = %v, want a run recorded at or after %v", last, before)
	}
}

// TestDutyStatePassBadLimit: with a refused gc limit the record is still
// written. The gc observation is unknown with the refused setting as its error,
// its record holds limit 0, the other duties keep their family limits, and the
// log holds only the pass line.
func TestDutyStatePassBadLimit(t *testing.T) {
	t.Parallel()
	d := liveDaemon(t, time.Now())
	d.patrolConfig.Patrols.Reconciler.Enabled = true
	d.patrolConfig.Patrols.Reconciler.GCLimit = "a while"
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)

	d.runReconciler()

	state, err := ReadDutyState(d.config.TownRoot)
	if err != nil {
		t.Fatalf("ReadDutyState after a pass with a refused gc limit = %v, want the record", err)
	}
	const want = "backup=ok/86400 archive=ok/86400 gc=unknown/0 town_state=ok/86400"
	if sig := dutyRecordSignature(state.Duties); sig != want {
		t.Errorf("duty state records = %q, want %q", sig, want)
	}
	if got := strings.TrimSpace(logs.String()); got != "reconciler: pass (shadow)" {
		t.Errorf("log = %q, want only the pass line", got)
	}
}

// dutyRecordSignature names each record as name=verdict/limitSeconds, in order.
func dutyRecordSignature(records []DutyRecord) string {
	parts := make([]string, 0, len(records))
	for _, r := range records {
		parts = append(parts, fmt.Sprintf("%s=%s/%d", r.Name, r.Verdict, r.LimitSeconds))
	}
	return strings.Join(parts, " ")
}
