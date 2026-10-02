package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/supervisor"
)

func TestPatrolScanDefaultsOff(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "patrol_scan") {
		t.Error("patrol_scan must be off with no config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "patrol_scan") {
		t.Error("patrol_scan must be off with no patrol_scan entry")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "patrol_scan") {
		t.Error("patrol_scan enabled:true must be on")
	}
}

func TestPatrolScanInterval(t *testing.T) {
	t.Parallel()
	if got := patrolScanInterval(nil); got != 2*time.Minute {
		t.Errorf("default = %v", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{IntervalStr: "90s"}}}
	if got := patrolScanInterval(cfg); got != 90*time.Second {
		t.Errorf("configured = %v", got)
	}
	cfg.Patrols.PatrolScan.IntervalStr = "bogus"
	if got := patrolScanInterval(cfg); got != 2*time.Minute {
		t.Errorf("bad interval = %v", got)
	}
}

// The patrol scan repeats the landing label to stay out of the land
// package's dependency tree; this pins the copy to the original.
func TestPatrolScanReadyToLandLabelMatchesLand(t *testing.T) {
	t.Parallel()
	if patrolscan.ReadyToLandLabel != land.LabelReadyToLand {
		t.Fatalf("patrolscan.ReadyToLandLabel = %q, land.LabelReadyToLand = %q", patrolscan.ReadyToLandLabel, land.LabelReadyToLand)
	}
}

// The tick and the daemon's crash detector each ask "is this bead submitted
// for landing?" from their own copy of the rule. gt-xs1ni was two detectors
// answering it differently, so this pins them to the same answer on every
// status a work bead can be in.
func TestPatrolScanIsSubmittedMatchesPolecat(t *testing.T) {
	t.Parallel()
	statuses := []string{"open", "hooked", "in_progress", "blocked", "deferred", "closed", "tombstone", "", "weird"}
	for _, status := range statuses {
		for _, labels := range [][]string{nil, {land.LabelReadyToLand}, {"rework"}, {land.LabelReadyToLand, "rework"}} {
			w := patrolscan.Work{Status: status, Labels: labels}
			got := w.IsSubmitted()
			want := polecat.IsSubmittedWork(&beads.Issue{Status: status, Labels: labels})
			if got != want {
				t.Errorf("status %q labels %v: patrolscan says %v, polecat says %v", status, labels, got, want)
			}
		}
	}
}

func TestPatrolScanOptionsUseDispatchHoldRule(t *testing.T) {
	t.Parallel()
	o := patrolScanOptions(nil, time.Now)
	if why := o.HoldReason(patrolscan.Work{Status: "hooked", Labels: []string{"needs-mayor-review"}}); why == "" {
		t.Error("needs-mayor-review work must read as held")
	}
	if why := o.HoldReason(patrolscan.Work{Status: "hooked"}); why != "" {
		t.Errorf("plain hooked work read as held: %q", why)
	}
	refused := errors.Join(supervisor.ErrRefused, supervisor.ErrBudgetExhausted)
	if !o.IsRefusal(refused) || o.IsRefusal(errors.New("exit status 1")) {
		t.Error("IsRefusal must match supervisor refusals only")
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{DeadSamples: 3, ReportWindow: "6h"}}}
	o = patrolScanOptions(cfg, time.Now)
	if o.DeadSamples != 3 || o.ReportWindow != 6*time.Hour {
		t.Errorf("options = %+v", o)
	}
}

func TestIssueWorkParsesMoleculeAndTime(t *testing.T) {
	t.Parallel()
	w := issueWork(&beads.Issue{
		ID: "gt-a", Status: "hooked", Assignee: "gastown/polecats/ruby",
		Description: "attached_molecule: gt-wisp-6nm\nattached_formula: mol-polecat-work\n",
		UpdatedAt:   "2026-09-30T11:30:06Z",
	})
	if w.AttachedMolecule != "gt-wisp-6nm" {
		t.Errorf("attached molecule = %q", w.AttachedMolecule)
	}
	if w.UpdatedAt.IsZero() {
		t.Error("updated_at not parsed")
	}
}

// A polecat restart in a rig patrol_scan does not cover is declined, so it
// spends no budget and the witness keeps that rig.
func TestRestartPolecatDeclinedOutsidePatrolScan(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: t.TempDir()}}
	err := d.restartSeat(supervisor.SeatFor("gastown", constants.RolePolecat, "ruby"))
	if !errors.Is(err, supervisor.ErrDeclined) {
		t.Fatalf("err = %v, want ErrDeclined", err)
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: true, Rigs: []string{"beads"}}}}
	if d.patrolScanActiveForRig("gastown") || !d.patrolScanActiveForRig("beads") {
		t.Fatal("rigs list not honoured")
	}
}

// gt done writes gt:ready-to-land before the intent record, and the record
// write is best-effort: a seat whose write was lost reads as run for the whole
// of the landing, and townhealth reports it dead on the samples the scan
// records while it is mid-landing (gt-2z8k1). Bringing the record up to the
// label ends both.
func TestMarkSubmittedBringsTheRecordUpToTheLabel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const name = "ruby"
	if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", name), 0o755); err != nil {
		t.Fatal(err)
	}
	iseat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: name}
	writeJSONFile(t, iseat.Path(town), intent.Record{
		Progress: &intent.Progress{SampledAt: time.Now().Add(-time.Minute), DeadSamples: 3},
	})

	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: town}}
	src := &healthSources{d: d, evidence: time.Hour, now: time.Now()}
	before, err := src.Seats()
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}
	if len(before) != 1 || before[0].DeadSamples != 3 {
		t.Fatalf("before = %+v, want the mid-landing seat reported dead", before)
	}

	if err := (&patrolScanHost{d: d}).MarkSubmitted("gastown", name, "gt-ruby"); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	rec, err := intent.Read(town, iseat)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Submitted() || rec.WorkBead != "gt-ruby" {
		t.Fatalf("record = %+v, want submitted for gt-ruby", rec)
	}
	if rec.Progress != nil {
		t.Fatalf("progress = %+v, want dropped with the submission", rec.Progress)
	}
	after, err := src.Seats()
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("after = %+v, want the submitted seat dropped", after)
	}
}

func TestTriggerPatrolScanSingleFlight(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	d.patrolScanRunning.Store(true)
	if d.triggerPatrolScan() {
		t.Fatal("a second tick started while one was running")
	}
	d.patrolScanRunning.Store(false)
	// Patrol off: the tick returns without scanning.
	if !d.triggerPatrolScan() {
		t.Fatal("tick did not start")
	}
	d.patrolScanCycles.Wait()
	if d.patrolScanRunning.Load() {
		t.Fatal("guard not released")
	}
}

// TestPatrolScanGHGatesHonorsTheRecordedRun pins the cadence: a gh gate check
// within the interval dispatches nothing, so bd is never started. The unit
// tier fails the run if an external tool starts, which is the assertion.
func TestPatrolScanGHGatesHonorsTheRecordedRun(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := savePatrolLastRun(townRoot, "patrol_scan_gh_gates", time.Now()); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: townRoot}}

	d.patrolScanGHGates(&patrolScanHost{d: d}, nil)
}

// A bead bd says does not exist is gone, not unknown: the seat's submitted
// record outlived it and nothing is waiting to land. Any other read failure
// is unknown, and the tick leaves the seat alone on it (gt-xs1ni).
func TestPatrolScanWorkBeadTellsGoneFromUnreadable(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: bd.open}
	h := &patrolScanHost{d: d}

	bd.seed("gt-live", "hooked", time.Now(), "gt:ready-to-land")
	if w, err := h.WorkBead("myr", "gt-live"); err != nil || w == nil || w.ID != "gt-live" {
		t.Fatalf("WorkBead(gt-live) = %+v, %v", w, err)
	}
	if w, err := h.WorkBead("myr", "gt-gone"); err != nil || w != nil {
		t.Fatalf("WorkBead(gt-gone) = %+v, %v; want nil, nil", w, err)
	}
	bd.showErr = errors.New("bd show: connection refused")
	if w, err := h.WorkBead("myr", "gt-live"); err == nil || w != nil {
		t.Fatalf("a failed read answered %+v, %v; want an error", w, err)
	}
}

// ClearSubmission ends the seat's wait for the landing worker. It is the
// action half of the stale-record fix: without it the supervisor's own
// submitted guard refuses every restart (gt-xs1ni).
func TestPatrolScanClearSubmissionEndsTheWait(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: townRoot}}
	h := &patrolScanHost{d: d}
	seat := supervisor.IntentSeat(supervisor.SeatFor("myr", constants.RolePolecat, "mycat"))
	if err := intent.MarkSubmitted(townRoot, seat, "gt-work1", "gt done", time.Now()); err != nil {
		t.Fatal(err)
	}

	changed, err := h.ClearSubmission("myr", "mycat", "gt-work1")
	if err != nil || !changed {
		t.Fatalf("ClearSubmission = %v, %v; want true, nil", changed, err)
	}
	rec, err := intent.Read(townRoot, seat)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Submitted() {
		t.Fatalf("record still says submitted: %+v", rec)
	}

	// A record for another bead is left alone: the seat is waiting for
	// something else's landing.
	if err := intent.MarkSubmitted(townRoot, seat, "gt-work2", "gt done", time.Now()); err != nil {
		t.Fatal(err)
	}
	if changed, err := h.ClearSubmission("myr", "mycat", "gt-work1"); err != nil || changed {
		t.Fatalf("ClearSubmission for another bead = %v, %v; want false, nil", changed, err)
	}
}
