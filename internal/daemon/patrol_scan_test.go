package daemon

import (
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/patrolscan"
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

func TestBdIssueWorkParsesMoleculeAndTime(t *testing.T) {
	t.Parallel()
	w := bdIssue{
		ID: "gt-a", Status: "hooked", Assignee: "gastown/polecats/ruby",
		Description: "attached_molecule: gt-wisp-6nm\nattached_formula: mol-polecat-work\n",
		UpdatedAt:   "2026-09-30T11:30:06Z",
	}.work()
	if w.AttachedMolecule != "gt-wisp-6nm" {
		t.Errorf("attached molecule = %q", w.AttachedMolecule)
	}
	if w.UpdatedAt.IsZero() {
		t.Error("updated_at not parsed")
	}
}

func TestWitnessWantedInRig(t *testing.T) {
	t.Parallel()
	if !WitnessWantedInRig(nil, "gastown") {
		t.Error("no config: witness wanted (default on)")
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{Witness: &PatrolConfig{Enabled: true, DisabledRigs: []string{"gastown"}}}}
	if WitnessWantedInRig(cfg, "gastown") {
		t.Error("disabled_rigs must switch the witness off in that rig")
	}
	if !WitnessWantedInRig(cfg, "beads") {
		t.Error("a rig not listed keeps its witness")
	}
	cfg.Patrols.Witness.Enabled = false
	if WitnessWantedInRig(cfg, "beads") {
		t.Error("witness enabled:false switches every rig off")
	}
}

func TestPatrolWatchdogSkipsSwitchedOffRoles(t *testing.T) {
	t.Parallel()
	targets := []patrolWatchdogTarget{
		{Role: constants.RoleDeacon},
		{Role: constants.RoleWitness, Rig: "gastown"},
		{Role: constants.RoleWitness, Rig: "beads"},
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		Deacon:  &PatrolConfig{Enabled: false},
		Witness: &PatrolConfig{Enabled: true, DisabledRigs: []string{"gastown"}},
	}}
	active := func(p string) bool { return IsPatrolEnabled(cfg, p) }
	got := filterPatrolWatchdogTargets(targets, cfg, active)
	if len(got) != 1 || got[0].Rig != "beads" {
		t.Fatalf("targets = %+v, want only the beads witness", got)
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
