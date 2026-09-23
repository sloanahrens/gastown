package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/guard"
)

func TestPatrolWatchdogInterval(t *testing.T) {
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
	targets := patrolWatchdogTargets("/town", []string{"gastown"})

	if len(targets) != 3 {
		t.Fatalf("expected 3 targets (deacon + witness/refinery for 1 rig), got %d", len(targets))
	}
	if targets[0].Role != "deacon" || targets[0].Rig != "" {
		t.Errorf("expected first target to be the town-level deacon, got %+v", targets[0])
	}
	if targets[0].Assignee != "deacon/" {
		t.Errorf("expected deacon assignee 'deacon/', got %q", targets[0].Assignee)
	}

	var sawWitness, sawRefinery bool
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
		case "refinery":
			sawRefinery = true
			if target.Assignee != "gastown/refinery" {
				t.Errorf("expected refinery assignee 'gastown/refinery', got %q", target.Assignee)
			}
		}
	}
	if !sawWitness || !sawRefinery {
		t.Errorf("expected both witness and refinery targets for the rig, got %+v", targets)
	}

	// Every target reads its patrol wisps from the TOWN database, rig-scoped
	// roles included: `gt patrol report` writes them with
	// BeadsDir=roleInfo.TownRoot (internal/cmd/patrol_report.go). A rig workdir
	// resolves through <rig>/.beads/redirect into the rig database, which holds
	// no patrol wisp at all, and the resulting empty result read as a resolved
	// "never patrolled" for every witness and refinery (hq-3h7ac).
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
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	targets := []patrolWatchdogTarget{stubTarget("refinery", "gastown")}

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
	key := patrolWatchdogAlertKey(stubTarget("deacon", ""))
	if key != "patrol_watchdog:deacon" {
		t.Errorf("unexpected alert key %q", key)
	}
}
