package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// TestCheckPolecatHealth_DetectsCrashedPolecat verifies that checkPolecatHealth
// does detect a crash for a polecat in agent_state=working with a dead session.
// This ensures the spawning guard in issue #1752 does not accidentally suppress
// legitimate crash detection for polecats that were running normally.
func TestCheckPolecatHealth_DetectsCrashedPolecat(t *testing.T) {
	t.Parallel()
	bd := hookedWorkBD(t, "gt-xyz", time.Hour)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        log.New(&logBuf, "", 0),
		tmux:          newFakeTmux(newFixedClock()),
		notifier:      notifyfake.New(),
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("expected CRASH DETECTED for working polecat with dead session, got: %q", got)
	}

	// The session_death event must land in the daemon's configured TownRoot,
	// not in a town root resolved from the test process's cwd (gt-x9o).
	eventsData, err := os.ReadFile(filepath.Join(townRoot, events.EventsFile))
	if err != nil {
		t.Fatalf("expected session_death event in configured TownRoot: %v", err)
	}
	if !strings.Contains(string(eventsData), events.TypeSessionDeath) {
		t.Errorf("events file missing session_death event: %q", eventsData)
	}
}

// TestCheckPolecatHealth_SkipsParkedPolecat pins the daemon's side of
// gt-fojqs. A dead session with an open hook is the crash signature, but on a
// polecat the operator deliberately stopped (gt session stop) or parked
// (gt agent pause) it is the state they asked for. The pause marker is the
// choke point every scanner honors (gt-ahik); without this gate the daemon
// raises CRASH DETECTED and a session_death event for an intentional stop.
// TestCheckPolecatHealth_DetectsCrashedPolecat is the control: same fakes,
// no marker, crash reported.
func TestCheckPolecatHealth_SkipsParkedPolecat(t *testing.T) {
	t.Parallel()
	bd := hookedWorkBD(t, "gt-xyz", time.Hour)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        log.New(&logBuf, "", 0),
		tmux:          newFakeTmux(newFixedClock()),
		notifier:      notifyfake.New(),
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}

	if err := agentpause.Pause(townRoot, "myr", constants.RolePolecat, "mycat", "deliberate stop (gt session stop)", "overseer", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("parked polecat must not trigger CRASH DETECTED, got: %q", got)
	}
	if !strings.Contains(got, "parked") {
		t.Errorf("expected log to say the polecat is parked, got: %q", got)
	}
	if _, err := os.Stat(filepath.Join(townRoot, events.EventsFile)); err == nil {
		t.Error("a parked polecat must not emit a session_death event")
	}
}

// TestCheckPolecatHealth_SpawningGuardExpires verifies that the spawn grace
// is time-bound: work hooked more than 5 minutes ago with no session is a
// crash (gt sling may have failed during spawn).
func TestCheckPolecatHealth_SpawningGuardExpires(t *testing.T) {
	t.Parallel()
	bd := hookedWorkBD(t, "gt-xyz", 10*time.Minute)

	var logBuf strings.Builder
	d := &Daemon{
		config:        &Config{TownRoot: t.TempDir()},
		logger:        log.New(&logBuf, "", 0),
		tmux:          newFakeTmux(newFixedClock()),
		notifier:      notifyfake.New(),
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}

	d.checkPolecatHealth("myr", "mycat")

	if got := logBuf.String(); !strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("expected CRASH DETECTED once the spawn grace has passed, got: %q", got)
	}
}

// TestCheckPolecatHealth_SkipsClosedHookBead verifies that checkPolecatHealth
// does NOT fire CRASHED_POLECAT when the hook_bead is already closed.
// This is the regression test for the false-positive spam bug (issue hq-1o7):
// when a polecat completes work normally, the hook_bead gets closed but the
// stale reference remains on the agent bead, causing repeated false alerts.
func TestCheckPolecatHealth_SkipsClosedHookBead(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	bd.seed("fe-xyz", "closed", time.Now())

	var logBuf strings.Builder
	d := &Daemon{
		config:        &Config{TownRoot: t.TempDir()},
		logger:        log.New(&logBuf, "", 0),
		tmux:          newFakeTmux(newFixedClock()),
		notifier:      notifyfake.New(),
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}
	if _, err := intent.Update(d.config.TownRoot, intent.Seat{Rig: "myr", Role: "polecat", Name: "mycat"}, func(r *intent.Record) error {
		r.WorkBead = "fe-xyz"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "hook_bead fe-xyz is already closed") {
		t.Errorf("expected log about closed hook_bead, got: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("closed hook_bead must not trigger CRASH DETECTED, got: %q", got)
	}
}

// TestCheckPolecatHealth_NoActiveWorkIsNotACrash: a polecat that finished
// (gt done closed its work) or was nuked has no hooked or in-progress work
// assigned, so its dead session is not a crash. The agent bead's done/nuked
// state used to carry this; the work bead does, without an agent-bead read.
func TestCheckPolecatHealth_NoActiveWorkIsNotACrash(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	var logBuf strings.Builder
	d := &Daemon{
		config:        &Config{TownRoot: t.TempDir()},
		logger:        log.New(&logBuf, "", 0),
		tmux:          newFakeTmux(newFixedClock()),
		notifier:      notifyfake.New(),
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}

	d.checkPolecatHealth("myr", "mycat")

	if got := logBuf.String(); strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("a polecat with no active work was called crashed: %q", got)
	}
}

// TestCheckPolecatHealth_CrashSendsNoMail verifies that a detected polecat
// crash is logged and left to patrol_scan: no CRASHED_POLECAT mail goes out,
// because the witness that read it is gone (gt-4k3fj.6.1).
func TestCheckPolecatHealth_CrashSendsNoMail(t *testing.T) {
	t.Parallel()
	bd := hookedWorkBD(t, "gt-xyz", time.Hour)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	notes := notifyfake.New()
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        log.New(&logBuf, "", 0),
		tmux:          newFakeTmux(newFixedClock()),
		notifier:      notes,
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "CRASH DETECTED") {
		t.Fatalf("expected CRASH DETECTED, got: %q", got)
	}
	if !strings.Contains(got, "patrol_scan does not cover rig myr") {
		t.Errorf("expected the uncovered-rig log line, got: %q", got)
	}
	if calls := notes.Calls(); len(calls) != 0 {
		t.Errorf("a crash sent notifications: %+v", calls)
	}
}

// stoppedOnEscalationBD returns a work-bead database on which myr/polecats/
// mycat's agent bead reads the record gt done's ESCALATED exit writes (stuck,
// exit ESCALATED, holding hooked bead), beside an escalation the seat raised
// for that bead in the given state.
func stoppedOnEscalationBD(t *testing.T, townRoot, bead, escalation, raisedBy string, escalationOpen bool) *workBD {
	t.Helper()
	bd := hookedWorkBD(t, bead, time.Hour)
	bd.db.Seed(beads.Issue{
		ID: beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, "myr"), "myr", "mycat"),
		Description: beads.FormatAgentDescription("mycat", &beads.AgentFields{
			RoleType: constants.RolePolecat, Rig: "myr", AgentState: "stuck", ExitType: done.ExitEscalated,
			HookBead: bead, LastSourceIssue: bead,
		}),
	})
	status := "open"
	if !escalationOpen {
		status = "closed"
	}
	title := bead + ": a decision the bead puts out of scope"
	bd.db.Seed(beads.Issue{
		ID: escalation, Status: status, Title: title, Labels: []string{"gt:escalation"},
		Description: beads.FormatEscalationDescription(title, &beads.EscalationFields{
			Severity: "high", EscalatedBy: raisedBy, EscalatedAt: time.Now().UTC().Format(time.RFC3339),
		}),
	})
	return bd
}

// A polecat that stopped on purpose to raise a decision is not a crash: its
// dead session with the bead still hooked is the state gt done's ESCALATED
// exit leaves, and the patrol scan owns the seat until the escalation that
// turn raised for the bead closes (gt-4hduq). The daemon says so once — the
// measured case logged CRASH DETECTED every three minutes for fourteen hours —
// and emits no session_death event.
func TestCheckPolecatHealth_HoldsASeatWaitingOnItsEscalation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		raisedBy       string
		escalationOpen bool
		wantLog        string
	}{
		{"open escalation", "myr/polecats/mycat", true, "hq-wisp-1 is still open for gt-xyz"},
		{"closed escalation", "myr/polecats/mycat", false, "hq-wisp-1 for gt-xyz is closed; patrol_scan resumes the seat"},
		{"another role's escalation for the bead", "myr/crew/sloan", false, "no escalation of the seat's names gt-xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			bd := stoppedOnEscalationBD(t, townRoot, "gt-xyz", "hq-wisp-1", tc.raisedBy, tc.escalationOpen)

			var logBuf strings.Builder
			d := &Daemon{
				config:        &Config{TownRoot: townRoot},
				logger:        log.New(&logBuf, "", 0),
				tmux:          newFakeTmux(newFixedClock()),
				notifier:      notifyfake.New(),
				openWorkBeads: bd.open,
				execCmd:       bd.run,
			}

			// Two passes: the hold is logged when it starts, not every pass.
			d.checkPolecatHealth("myr", "mycat")
			d.checkPolecatHealth("myr", "mycat")

			got := logBuf.String()
			if strings.Contains(got, "CRASH DETECTED") {
				t.Errorf("a seat waiting on its escalation was called crashed: %q", got)
			}
			if n := strings.Count(got, "Skipping crash detection"); n != 1 {
				t.Errorf("hold lines = %d, want one over two passes: %q", n, got)
			}
			if !strings.Contains(got, tc.wantLog) {
				t.Errorf("log = %q, want it to say %q", got, tc.wantLog)
			}
			if _, err := os.Stat(filepath.Join(townRoot, events.EventsFile)); err == nil {
				t.Error("a seat waiting on a human decision must not emit a session_death event")
			}
		})
	}
}

// The control: the hold is the ESCALATED record's doing, not the escalation
// bead's. The same seat with its escalation still open but no exit type
// recorded is an ordinary crash, and crash detection goes on as before.
func TestCheckPolecatHealth_HoldNeedsTheEscalatedRecord(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	bd := stoppedOnEscalationBD(t, townRoot, "gt-xyz", "hq-wisp-1", "myr/polecats/mycat", true)
	bd.db.Seed(beads.Issue{
		ID: beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, "myr"), "myr", "mycat"),
		Description: beads.FormatAgentDescription("mycat", &beads.AgentFields{
			RoleType: constants.RolePolecat, Rig: "myr", AgentState: "working", HookBead: "gt-xyz",
		}),
	})

	var logBuf strings.Builder
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        log.New(&logBuf, "", 0),
		tmux:          newFakeTmux(newFixedClock()),
		notifier:      notifyfake.New(),
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}

	d.checkPolecatHealth("myr", "mycat")

	if got := logBuf.String(); !strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("a working polecat with a dead session is still a crash: %q", got)
	}
}

// hookedWorkBD returns a work-bead database on which work bead id is hooked
// to myr/polecats/mycat, last updated ago.
func hookedWorkBD(t *testing.T, id string, ago time.Duration) *workBD {
	t.Helper()
	bd := newWorkBD(t)
	bd.seed(id, "hooked", time.Now().Add(-ago))
	return bd
}

// polecatSessionTmux returns a fake tmux holding the polecat session
// "myr-mycat", its pane running paneCommand ("bash" for an idle shell),
// created at created.
func polecatSessionTmux(paneCommand string, created time.Time) *fakeTmux {
	tm := newFakeTmux(newFixedClock())
	tm.addSession("myr-mycat", paneCommand, created)
	return tm
}

// lookupFailBD is a work-bead database with no agent beads (an agent-bead
// lookup finds nothing): with hasWork, an open work bead assigned to
// myr/polecats/mycat; otherwise nothing.
func lookupFailBD(t *testing.T, hasWork bool) *workBD {
	t.Helper()
	bd := newWorkBD(t)
	if hasWork {
		bd.seed("wh-test-1", "open", time.Now())
	}
	return bd
}

// TestReapIdlePolecat_SkipsWhenBeadLookupFailsButHasWork verifies that reapIdlePolecat
// does NOT kill a polecat when the agent bead lookup fails but hasAssignedOpenWork
// confirms the polecat has an open work bead assigned. This is the regression test
// for the working-bead-lookup-failed kill bug (GH#3342 followup).
func TestReapIdlePolecat_SkipsWhenBeadLookupFailsButHasWork(t *testing.T) {
	t.Parallel()

	bd := lookupFailBD(t, true /* hasWork */)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		prefixRegistryFn: myrPrefixes,
		config:           &Config{TownRoot: townRoot},
		logger:           log.New(&logBuf, "", 0),
		tmux:             polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier:         notifyfake.New(),
		openWorkBeads:    bd.open,
		execCmd:          bd.run,
	}

	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	_ = os.MkdirAll(filepath.Dir(hbPath), 0755)
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-60 * time.Minute),
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	if strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Errorf("must NOT reap polecat with open assigned work when agent bead lookup fails, got: %q", logBuf.String())
	}
}

// TestReapIdlePolecat_ReapsWhenBeadLookupFailsAndNoWork verifies that reapIdlePolecat
// DOES kill a polecat when the agent bead lookup fails, no work is assigned, and the
// agent process is not running. Ensures the hasAssignedOpenWork guard doesn't over-protect.
func TestReapIdlePolecat_ReapsWhenBeadLookupFailsAndNoWork(t *testing.T) {
	t.Parallel()

	bd := lookupFailBD(t, false /* no work */)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		prefixRegistryFn: myrPrefixes,
		config:           &Config{TownRoot: townRoot},
		logger:           log.New(&logBuf, "", 0),
		tmux:             polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier:         notifyfake.New(),
		openWorkBeads:    bd.open,
		execCmd:          bd.run,
	}

	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	_ = os.MkdirAll(filepath.Dir(hbPath), 0755)
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-45 * time.Minute), // 3x the 15m timeout
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	if !strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Errorf("expected idle polecat with no work and failed bead lookup to be reaped, got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "working-no-hook") {
		t.Errorf("expected working-no-hook reason, got: %q", logBuf.String())
	}
	if alive, _ := d.tmux.HasSession("myr-mycat"); alive {
		t.Error("the reaped polecat's session is still alive")
	}
}

// TestReapIdlePolecat_SkipsActiveAgent verifies that reapIdlePolecat does NOT kill
// a polecat whose hook_bead is missing but whose agent process is still running.
// This is the regression test for GH#3342: a failed gt sling rollback can clear
// the hook while the agent is actively working, causing the daemon to incorrectly
// reap the session.
func TestReapIdlePolecat_SkipsActiveAgent(t *testing.T) {
	// Register "myr" prefix so session name resolves to "myr-mycat"
	t.Parallel()

	// No work bead is assigned (a failed sling rollback cleared the hook).
	bd := newWorkBD(t)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		prefixRegistryFn: myrPrefixes,
		config:           &Config{TownRoot: townRoot},
		logger:           log.New(&logBuf, "", 0),
		tmux:             polecatSessionTmux("codex", time.Now().Add(-time.Hour)),
		notifier:         notifyfake.New(),
		openWorkBeads:    bd.open,
		execCmd:          bd.run,
	}

	// Write a stale heartbeat (working state, 20 minutes old) so the reaper considers it
	polecat.TouchSessionHeartbeatWithState(townRoot, "myr-mycat", polecat.HeartbeatWorking, "", "")
	// Backdate the heartbeat to make it stale
	hbPath := filepath.Join(townRoot, "heartbeats", "myr-mycat.json")
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-20 * time.Minute),
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	got := logBuf.String()
	if strings.Contains(got, "Reaping idle polecat") {
		t.Errorf("must NOT reap polecat with active agent process (GH#3342), got: %q", got)
	}
}

// TestReapIdlePolecat_ReapsIdleNoHook verifies that reapIdlePolecat DOES kill
// a polecat whose hook_bead is missing AND whose agent process is NOT running
// (idle shell). This ensures the GH#3342 fix doesn't prevent legitimate reaping.
func TestReapIdlePolecat_ReapsIdleNoHook(t *testing.T) {
	// Register "myr" prefix so session name resolves to "myr-mycat"
	t.Parallel()

	// No work bead is assigned (a failed sling rollback cleared the hook).
	bd := newWorkBD(t)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		prefixRegistryFn: myrPrefixes,
		config:           &Config{TownRoot: townRoot},
		logger:           log.New(&logBuf, "", 0),
		tmux:             polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier:         notifyfake.New(),
		openWorkBeads:    bd.open,
		execCmd:          bd.run,
	}

	// Write a stale heartbeat (working state, 20 minutes old) so the reaper considers it
	polecat.TouchSessionHeartbeatWithState(townRoot, "myr-mycat", polecat.HeartbeatWorking, "", "")
	// Backdate the heartbeat to make it stale
	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-20 * time.Minute),
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	got := logBuf.String()
	if !strings.Contains(got, "Reaping idle polecat") {
		t.Errorf("expected idle polecat with no agent to be reaped, got: %q", got)
	}
	if !strings.Contains(got, "working-no-hook") {
		t.Errorf("expected working-no-hook reason, got: %q", got)
	}
	if alive, _ := d.tmux.HasSession("myr-mycat"); alive {
		t.Error("the reaped polecat's session is still alive")
	}
}

// TestReapIdlePolecat_SkipsFreshSessionWithStaleInheritedHeartbeat verifies that
// reapIdlePolecat does NOT kill a session that tmux reports as freshly created,
// even when the heartbeat file is very stale. Regression test for gt-5mkr: a
// reused polecat name inherits the PREVIOUS incarnation's heartbeat file
// (state=exiting, hours old) until the new incarnation writes its own — the
// reaper must not treat that inherited staleness as the new session's idle time.
func TestReapIdlePolecat_SkipsFreshSessionWithStaleInheritedHeartbeat(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		prefixRegistryFn: myrPrefixes,
		config:           &Config{TownRoot: townRoot},
		logger:           log.New(&logBuf, "", 0),
		tmux:             polecatSessionTmux("bash", time.Now().Add(-3*time.Minute)),
		notifier:         notifyfake.New(),
	}

	// Heartbeat left behind by a PREVIOUS incarnation of this name: state=exiting,
	// ~4h stale — matches the observed "state=exiting, idle 3h48m" bug report.
	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	if err := os.MkdirAll(filepath.Dir(hbPath), 0755); err != nil {
		t.Fatalf("creating heartbeats dir: %v", err)
	}
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-4 * time.Hour),
		State:     polecat.HeartbeatExiting,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	got := logBuf.String()
	if strings.Contains(got, "Reaping idle polecat") {
		t.Errorf("must NOT reap a freshly-created session with a stale inherited heartbeat (gt-5mkr), got: %q", got)
	}
}

// TestReapIdlePolecat_SkipsPolecatRenewingExitingHeartbeat is the gt-azmw
// regression test at the daemon boundary. A polecat 19 minutes into a gt done
// whose default test-verify gate is still running is holding the container-gate
// slot (20m) and then running the suite (10m) — silent the whole way, with no
// gt subcommand left to re-touch the heartbeat via persistentPreRun. The daemon
// must not read that silence as an abandoned session and kill the process: that
// kill destroyed gastown/amethyst's in-flight MR on 2026-09-16.
//
// Both halves of this test matter. The first fails if the keep-alive stops
// renewing (a 19m-stale heartbeat is past the 15m threshold, so the reaper
// fires). The second fails for the tempting wrong fix — "never reap a polecat
// in state=exiting" — because once the gt done process is gone and nothing is
// renewing, the session really is abandoned and the reaper must still reclaim
// the API slot.
func TestReapIdlePolecat_SkipsPolecatRenewingExitingHeartbeat(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	var logBuf strings.Builder
	bd := newWorkBD(t)
	d := &Daemon{
		prefixRegistryFn: myrPrefixes,
		config:           &Config{TownRoot: townRoot},
		logger:           log.New(&logBuf, "", 0),
		tmux:             polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier:         notifyfake.New(),
		openWorkBeads:    bd.open,
		execCmd:          bd.run,
	}

	sessionName := session.PolecatSessionName(myrPrefixes().PrefixForRig("myr"), "mycat")
	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", sessionName+".json")
	writeStaleExitingHeartbeat := func(t *testing.T) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(hbPath), 0755); err != nil {
			t.Fatalf("creating heartbeats dir: %v", err)
		}
		// gt done's own write at the start of the flow, now 19m old: the gate
		// has been running past the 15m reap threshold without a single
		// further heartbeat.
		hb := polecat.SessionHeartbeat{
			Timestamp: time.Now().UTC().Add(-19 * time.Minute),
			State:     polecat.HeartbeatExiting,
			Context:   "gt done",
			Bead:      "gt-azmw",
		}
		data, _ := json.Marshal(hb)
		if err := os.WriteFile(hbPath, data, 0644); err != nil {
			t.Fatalf("writing heartbeat: %v", err)
		}
	}

	writeStaleExitingHeartbeat(t)

	// The fix: gt done renews the heartbeat for as long as its bounded gate
	// stage can run, so the polecat no longer looks abandoned from inside it.
	stop := polecat.StartExitingHeartbeatKeepAlive(townRoot, sessionName, "gt done", "gt-azmw")

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)
	if strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Fatalf("must NOT reap a polecat whose gt done is renewing the exiting heartbeat (gt-azmw), got: %q", logBuf.String())
	}

	// The gt done process is gone and nothing renews the heartbeat now — the
	// session is genuinely abandoned and must still be reclaimable.
	stop()
	writeStaleExitingHeartbeat(t)
	logBuf.Reset()

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)
	if !strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Fatalf("expected an unrenewed stale exiting polecat to still be reaped, got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), string(polecat.HeartbeatExiting)) {
		t.Fatalf("expected reap reason %q, got: %q", polecat.HeartbeatExiting, logBuf.String())
	}
}

// gt-7lwft: the reaper read a polecat deliberately waiting on a red main gate
// as idle and killed it, stranding committed work until a restart. gt done
// writes "exiting" at the top of its flow, so the run that exits 15 on the red
// gate leaves the same heartbeat as one that submitted — but not the same work
// bead. A stale idle or exiting heartbeat that still holds assigned work is not
// idle, and killing it is what strands the work.
func TestReapIdlePolecat_SkipsPolecatHoldingWork(t *testing.T) {
	t.Parallel()

	for _, state := range []polecat.HeartbeatState{polecat.HeartbeatExiting, polecat.HeartbeatIdle} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			bd := hookedWorkBD(t, "gt-work1", time.Hour)
			d, logBuf := reaperDaemon(t, bd)
			writePolecatHeartbeat(t, d.config.TownRoot, state, time.Hour)

			d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

			if alive, _ := d.tmux.HasSession("myr-mycat"); !alive {
				t.Fatalf("reaped a %s polecat that still holds work; log: %s", state, logBuf)
			}
			if !strings.Contains(logBuf.String(), "still holds assigned work") {
				t.Fatalf("the hold was not logged for %s: %s", state, logBuf)
			}
		})
	}
}

// The control for the test above: the same stale exiting heartbeat with no
// work assigned is still reclaimed, so the hold cannot be satisfied by "never
// reap a polecat in state=exiting" (gt-azmw).
func TestReapIdlePolecat_ReapsPolecatWithoutWork(t *testing.T) {
	t.Parallel()

	bd := newWorkBD(t)
	d, logBuf := reaperDaemon(t, bd)
	writePolecatHeartbeat(t, d.config.TownRoot, polecat.HeartbeatExiting, time.Hour)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	if !strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Fatalf("a stale, workless, agentless polecat was not reaped: %s", logBuf)
	}
	if alive, _ := d.tmux.HasSession("myr-mycat"); alive {
		t.Fatal("the reaped polecat's session is still alive")
	}
}

// gt-fcxe9.1: an unanswerable liveness query is UNKNOWN, not dead. With the agent
// bead unreadable and no assigned work, a confirmed-dead agent is reaped at 2x
// the idle threshold; an unknown one must wait for the 3x ceiling like a live
// one. The session's pane runs a shell, so the old error-dropping check read
// the failure as dead and reaped at 2.5x.
func TestReapIdlePolecat_UnknownLivenessIsNotDead(t *testing.T) {
	t.Parallel()

	bd := lookupFailBD(t, false /* no work */)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	tm := polecatSessionTmux("bash", time.Now().Add(-time.Hour))
	tm.setAliveErr("myr-mycat", fmt.Errorf("tmux show-environment: timed out"))
	d := &Daemon{
		prefixRegistryFn: myrPrefixes,
		config:           &Config{TownRoot: townRoot},
		logger:           log.New(&logBuf, "", 0),
		tmux:             tm,
		notifier:         notifyfake.New(),
		openWorkBeads:    bd.open,
		execCmd:          bd.run,
	}

	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	_ = os.MkdirAll(filepath.Dir(hbPath), 0755)
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-38 * time.Minute), // 2.5x the 15m timeout
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	if strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Fatalf("reaped a polecat whose liveness is unknown: %q", logBuf.String())
	}
	if alive, _ := d.tmux.HasSession("myr-mycat"); !alive {
		t.Fatal("session was killed on an unknown liveness answer")
	}
}

// gt-obbx2: gt done submits the branch, the session exits, and the hook still
// holds the bead until the landing worker lands it. Dead session + open hook is
// the crash signature, but here it is a polecat that finished: the witness
// raised a second session on it twice on 2026-09-30. Both halves are pinned —
// the intent record gt done writes, and the bead's gt:ready-to-land label for
// a seat whose record was never written.
//
// gt-xs1ni: the intent record is a wait, and the wait is only real while the
// bead it names still reads as submitted. gt done writes the label before the
// record, so a bead that lost the label was pulled after the submission — for
// rework, or by a human — and the record outlived it. That seat is a crashed
// rework and has to be reported, not skipped forever.
func TestCheckPolecatHealth_SkipsSubmittedWork(t *testing.T) {
	t.Parallel()
	t.Run("intent record, bead still waiting", func(t *testing.T) {
		t.Parallel()
		bd := newWorkBD(t)
		bd.seed("gt-work1", "hooked", time.Now().Add(-time.Hour), "gt:ready-to-land")
		d, logBuf := reaperDaemon(t, bd)
		d.tmux = newFakeTmux(newFixedClock())
		seat := intent.Seat{Rig: "myr", Role: "polecat", Name: "mycat"}
		if err := intent.MarkSubmitted(d.config.TownRoot, seat, "gt-work1", "gt done", time.Now()); err != nil {
			t.Fatal(err)
		}

		d.checkPolecatHealth("myr", "mycat")

		got := logBuf.String()
		if strings.Contains(got, "CRASH DETECTED") || !strings.Contains(got, "submitted for landing") {
			t.Fatalf("a submitted polecat was called crashed: %s", got)
		}
		if calls := bd.calls(t); !strings.Contains(calls, "show gt-work1") {
			t.Fatalf("the record was trusted without reading the bead it names, got calls:\n%s", calls)
		}
	})

	t.Run("intent record, landing pulled for rework", func(t *testing.T) {
		t.Parallel()
		bd := newWorkBD(t)
		bd.seed("gt-work1", "hooked", time.Now().Add(-time.Hour), "rework")
		d, logBuf := reaperDaemon(t, bd)
		d.tmux = newFakeTmux(newFixedClock())
		seat := intent.Seat{Rig: "myr", Role: "polecat", Name: "mycat"}
		if err := intent.MarkSubmitted(d.config.TownRoot, seat, "gt-work1", "gt done", time.Now()); err != nil {
			t.Fatal(err)
		}

		d.checkPolecatHealth("myr", "mycat")

		got := logBuf.String()
		if !strings.Contains(got, "CRASH DETECTED") {
			t.Fatalf("a seated rework whose landing was pulled was read as healthy: %s", got)
		}
	})

	t.Run("intent record, bead unreadable", func(t *testing.T) {
		t.Parallel()
		bd := newWorkBD(t)
		bd.seed("gt-work1", "hooked", time.Now().Add(-time.Hour), "rework")
		bd.showErr = errors.New("bd show: connection refused")
		d, logBuf := reaperDaemon(t, bd)
		d.tmux = newFakeTmux(newFixedClock())
		seat := intent.Seat{Rig: "myr", Role: "polecat", Name: "mycat"}
		if err := intent.MarkSubmitted(d.config.TownRoot, seat, "gt-work1", "gt done", time.Now()); err != nil {
			t.Fatal(err)
		}

		d.checkPolecatHealth("myr", "mycat")

		got := logBuf.String()
		if strings.Contains(got, "CRASH DETECTED") || !strings.Contains(got, "submitted for landing") {
			t.Fatalf("a failed bead read was taken as proof the landing ended: %s", got)
		}
	})

	t.Run("bead label", func(t *testing.T) {
		t.Parallel()
		bd := newWorkBD(t)
		bd.seed("gt-work1", "hooked", time.Now().Add(-time.Hour), "gt:ready-to-land")
		d, logBuf := reaperDaemon(t, bd)
		d.tmux = newFakeTmux(newFixedClock())

		d.checkPolecatHealth("myr", "mycat")

		got := logBuf.String()
		if strings.Contains(got, "CRASH DETECTED") || !strings.Contains(got, "submitted for landing") {
			t.Fatalf("a bead labeled gt:ready-to-land was called crashed: %s", got)
		}
	})
}
