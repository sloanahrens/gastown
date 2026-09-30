package daemon

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/guard"
	"github.com/steveyegge/gastown/internal/rig"
)

func TestPatrolWatchdogInterval(t *testing.T) {
	t.Parallel()
	if got := patrolWatchdogInterval(nil); got != defaultPatrolWatchdogInterval {
		t.Errorf("expected default interval %v, got %v", defaultPatrolWatchdogInterval, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			PatrolWatchdog: &PatrolWatchdogConfig{Enabled: true, IntervalStr: "5m"},
		},
	}
	if got := patrolWatchdogInterval(config); got != 5*time.Minute {
		t.Errorf("expected 5m interval, got %v", got)
	}

	for _, invalid := range []string{"invalid", "0", "-5m"} {
		config.Patrols.PatrolWatchdog.IntervalStr = invalid
		if got := patrolWatchdogInterval(config); got != defaultPatrolWatchdogInterval {
			t.Errorf("interval %q: expected default %v, got %v", invalid, defaultPatrolWatchdogInterval, got)
		}
	}
}

func TestPatrolWatchdogCadenceAndMultiplier_Defaults(t *testing.T) {
	t.Parallel()
	if got := patrolWatchdogCadence(nil); got != defaultPatrolWatchdogCadence {
		t.Errorf("expected default cadence %v, got %v", defaultPatrolWatchdogCadence, got)
	}
	if got := patrolWatchdogMultiplier(nil); got != defaultPatrolWatchdogMultiplier {
		t.Errorf("expected default multiplier %d, got %d", defaultPatrolWatchdogMultiplier, got)
	}
	if !patrolWatchdogNudgeEnabled(nil) {
		t.Error("expected nudge to default on with nil config")
	}
}

func TestPatrolWatchdogCadenceAndMultiplier_Configured(t *testing.T) {
	t.Parallel()
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			PatrolWatchdog: &PatrolWatchdogConfig{Enabled: true, CadenceStr: "20m", Multiplier: 5},
		},
	}
	if got := patrolWatchdogCadence(config); got != 20*time.Minute {
		t.Errorf("expected 20m cadence, got %v", got)
	}
	if got := patrolWatchdogMultiplier(config); got != 5 {
		t.Errorf("expected multiplier 5, got %d", got)
	}
}

func TestPatrolWatchdogNudgeEnabled_ExplicitFalse(t *testing.T) {
	t.Parallel()
	off := false
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			PatrolWatchdog: &PatrolWatchdogConfig{Enabled: true, Nudge: &off},
		},
	}
	if patrolWatchdogNudgeEnabled(config) {
		t.Error("expected nudge disabled when explicitly set false")
	}
}

// TestIsPatrolEnabled_PatrolWatchdogDefaultsOn: the failure this patrol
// exists for is silence — a role that looks alive but isn't patrolling — so
// like mayor_dispatch it must not need an opt-in to run.
func TestIsPatrolEnabled_PatrolWatchdogDefaultsOn(t *testing.T) {
	t.Parallel()
	if !IsPatrolEnabled(nil, "patrol_watchdog") {
		t.Error("expected patrol_watchdog to be enabled with a nil config")
	}
	config := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if !IsPatrolEnabled(config, "patrol_watchdog") {
		t.Error("expected patrol_watchdog to be enabled with no explicit config entry")
	}
	config.Patrols.PatrolWatchdog = &PatrolWatchdogConfig{Enabled: false}
	if IsPatrolEnabled(config, "patrol_watchdog") {
		t.Error("expected patrol_watchdog disabled when explicitly configured off")
	}
}

func TestPatrolWatchdogTargets_DeaconPlusEachRig(t *testing.T) {
	t.Parallel()
	targets := patrolWatchdogTargets("/town", []string{"gastown"})

	if len(targets) != 2 {
		t.Fatalf("expected 2 targets (deacon + witness for 1 rig), got %d", len(targets))
	}
	if targets[0].Role != "deacon" || targets[0].Rig != "" {
		t.Errorf("expected first target to be the town-level deacon, got %+v", targets[0])
	}
	if targets[0].Assignee != "deacon/" {
		t.Errorf("expected deacon assignee 'deacon/', got %q", targets[0].Assignee)
	}

	var sawWitness bool
	for _, target := range targets[1:] {
		if target.Rig != "gastown" {
			t.Errorf("expected rig 'gastown', got %q", target.Rig)
		}
		switch target.Role {
		case "witness":
			sawWitness = true
			if target.Assignee != "gastown/witness" {
				t.Errorf("expected witness assignee 'gastown/witness', got %q", target.Assignee)
			}
		}
	}
	if !sawWitness {
		t.Errorf("expected a witness target for the rig, got %+v", targets)
	}

	// Every target reads its patrol wisps from the TOWN database, rig-scoped
	// roles included: `gt patrol report` writes them with
	// BeadsDir=roleInfo.TownRoot (internal/cmd/patrol_report.go). A rig workdir
	// resolves through <rig>/.beads/redirect into the rig database, which holds
	// no patrol wisp at all, and the resulting empty result read as a resolved
	// "never patrolled" for every witness (hq-3h7ac).
	for _, target := range targets {
		if target.WorkDir != "/town" {
			t.Errorf("%s %s: expected WorkDir to be the town root %q, got %q",
				target.Rig, target.Role, "/town", target.WorkDir)
		}
	}
}

func stubTarget(role, rig string) patrolWatchdogTarget {
	return patrolWatchdogTarget{
		Role:      role,
		Rig:       rig,
		Session:   role + "-session",
		Assignee:  role + "/",
		PatrolMol: "mol-" + role + "-patrol",
		WorkDir:   "/town",
	}
}

// TestAssessPatrolWatchdogTargets_StaleAliveRole_DrivesTheAlarm is the
// end-to-end drive: a live session whose bd-reported last completed patrol
// is far past the threshold must come back as a Fail finding. No real
// tmux/bd/mail is touched — both readers are injected fakes.
func TestAssessPatrolWatchdogTargets_StaleAliveRole_DrivesTheAlarm(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	targets := []patrolWatchdogTarget{stubTarget("witness", "gastown")}

	findings := assessPatrolWatchdogTargets(
		targets,
		func(patrolWatchdogTarget) bool { return true }, // session alive
		func(patrolWatchdogTarget) (time.Time, guard.Result) {
			return now.Add(-3 * 24 * time.Hour), guard.Pass() // last cycle 3 days ago
		},
		10*time.Minute, 3, now,
	)

	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if !findings[0].Result.IsFail() {
		t.Fatalf("expected Fail for a stale patrol on a live session, got %s", findings[0].Result)
	}

	msg := patrolWatchdogEscalationMessage(findings[0])
	if msg == "" {
		t.Fatal("expected a non-empty escalation message for a Fail finding")
	}
	key := patrolWatchdogAlertKey(findings[0].Target)
	if key != "patrol_watchdog:gastown/witness" {
		t.Errorf("unexpected alert key %q", key)
	}
}

func TestAssessPatrolWatchdogTargets_DeadSession_NoAlarm(t *testing.T) {
	t.Parallel()
	now := time.Now()
	targets := []patrolWatchdogTarget{stubTarget("witness", "gastown")}

	receiptReaderCalled := false
	findings := assessPatrolWatchdogTargets(
		targets,
		func(patrolWatchdogTarget) bool { return false }, // session dead
		func(patrolWatchdogTarget) (time.Time, guard.Result) {
			receiptReaderCalled = true
			return time.Time{}, guard.Fail("should not be called")
		},
		10*time.Minute, 3, now,
	)

	if !findings[0].Result.IsPass() {
		t.Fatalf("expected Pass for a dead session, got %s", findings[0].Result)
	}
	if receiptReaderCalled {
		t.Error("expected the receipt reader to be skipped for a dead session")
	}
}

// TestAssessPatrolWatchdogTargets_UnreadableReceipt_IsUnknown proves the
// end-to-end path also honors "an unreadable receipt is Unknown, never
// healthy" — a bd failure on a live session must not surface as Pass.
func TestAssessPatrolWatchdogTargets_UnreadableReceipt_IsUnknown(t *testing.T) {
	t.Parallel()
	now := time.Now()
	targets := []patrolWatchdogTarget{stubTarget("deacon", "")}

	findings := assessPatrolWatchdogTargets(
		targets,
		func(patrolWatchdogTarget) bool { return true },
		func(patrolWatchdogTarget) (time.Time, guard.Result) {
			return time.Time{}, guard.Unknown(errors.New("bd: connection refused"))
		},
		10*time.Minute, 3, now,
	)

	if !findings[0].Result.IsUnknown() {
		t.Fatalf("expected Unknown, got %s", findings[0].Result)
	}
}

func TestAssessPatrolWatchdogTargets_FreshAliveRole_Passes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	targets := []patrolWatchdogTarget{stubTarget("witness", "gastown")}

	findings := assessPatrolWatchdogTargets(
		targets,
		func(patrolWatchdogTarget) bool { return true },
		func(patrolWatchdogTarget) (time.Time, guard.Result) {
			return now.Add(-2 * time.Minute), guard.Pass()
		},
		10*time.Minute, 3, now,
	)

	if !findings[0].Result.IsPass() {
		t.Fatalf("expected Pass for a fresh patrol, got %s", findings[0].Result)
	}
}

func TestPatrolWatchdogAlertKey_TownLevelRole(t *testing.T) {
	t.Parallel()
	key := patrolWatchdogAlertKey(stubTarget("deacon", ""))
	if key != "patrol_watchdog:deacon" {
		t.Errorf("unexpected alert key %q", key)
	}
}

// TestPartitionPausedRigs_SkipsParkedAndDocked is the fix for gt-7g14a: a
// parked rig has no witness, so the watchdog must not build
// targets for it, while an unparked rig beside it is untouched.
func TestPartitionPausedRigs_SkipsParkedAndDocked(t *testing.T) {
	t.Parallel()
	states := map[string]rig.OpState{
		"working":  rig.OpStateOperational,
		"hm":       rig.OpStateParked,
		"mango":    rig.OpStateDocked,
		"neverwas": rig.OpStateOperational,
	}

	active, paused := partitionPausedRigs(
		[]string{"working", "hm", "mango", "neverwas"},
		func(rigName string) (rig.OpState, string) {
			return states[rigName], "test"
		},
	)

	if want := []string{"working", "neverwas"}; !reflect.DeepEqual(active, want) {
		t.Errorf("active = %v, want %v", active, want)
	}
	want := []pausedRig{{Rig: "hm", State: rig.OpStateParked}, {Rig: "mango", State: rig.OpStateDocked}}
	if !reflect.DeepEqual(paused, want) {
		t.Errorf("paused = %v, want %v", paused, want)
	}

	// The point of the split: a parked rig's roles never reach the target list,
	// so nothing reads their sessions, receipts, alerts or nudges.
	targets := patrolWatchdogTargets("/town", active)
	for _, target := range targets {
		if target.Rig == "hm" || target.Rig == "mango" {
			t.Errorf("parked/docked rig %q still produced a %s target", target.Rig, target.Role)
		}
	}
	if len(targets) != 1+len(active) {
		t.Errorf("expected the deacon plus one witness per active rig, got %d targets", len(targets))
	}
}

// TestPausedRigLog_OneNoticePerRigPerInterval pins the log budget the fix asks
// for: one line per skipped rig per hour, with a state change reported at once.
func TestPausedRigLog_OneNoticePerRigPerInterval(t *testing.T) {
	t.Parallel()
	var l pausedRigLog
	start := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	notice, entered := l.observe("hm", rig.OpStateParked, start)
	if !notice || !entered {
		t.Fatalf("first observation: notice=%v entered=%v, want true/true", notice, entered)
	}

	// Every cycle for the next hour is silent — the runaway line gt-7g14a
	// reports is exactly these repeats.
	for _, at := range []time.Time{start.Add(time.Minute), start.Add(30 * time.Minute)} {
		if notice, entered := l.observe("hm", rig.OpStateParked, at); notice || entered {
			t.Errorf("at %s: notice=%v entered=%v, want false/false", at, notice, entered)
		}
	}

	if notice, entered := l.observe("hm", rig.OpStateParked, start.Add(61*time.Minute)); !notice || entered {
		t.Errorf("past the interval: notice=%v entered=%v, want true/false", notice, entered)
	}

	// A different paused state is news, and so is the rig coming back.
	if notice, entered := l.observe("hm", rig.OpStateDocked, start.Add(62*time.Minute)); !notice || !entered {
		t.Errorf("state change: notice=%v entered=%v, want true/true", notice, entered)
	}
	l.forget("hm")
	if notice, entered := l.observe("hm", rig.OpStateParked, start.Add(63*time.Minute)); !notice || !entered {
		t.Errorf("after unpark and re-park: notice=%v entered=%v, want true/true", notice, entered)
	}

	// Rigs are throttled independently: mango's first line is never suppressed
	// by hm's.
	if notice, entered := l.observe("mango", rig.OpStateParked, start); !notice || !entered {
		t.Errorf("second rig: notice=%v entered=%v, want true/true", notice, entered)
	}
}

// TestPatrolWatchdogRigAlertKeys covers the keys a paused rig's transition
// clears — the rig's witness patrol, and only that rig's.
func TestPatrolWatchdogRigAlertKeys(t *testing.T) {
	t.Parallel()
	got := patrolWatchdogRigAlertKeys("hm")
	want := []string{"patrol_watchdog:hm/witness"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}
