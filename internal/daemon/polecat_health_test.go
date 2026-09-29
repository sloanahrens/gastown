package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// writeFakeTestTmux creates a shell script in dir named "tmux" that simulates
// "session not found" for has-session calls and fails on anything else.
func writeFakeTestTmux(t *testing.T, dir string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *has-session*) echo \"can't find session\" >&2; exit 1;;\n" +
		"  *) echo 'unexpected tmux command' >&2; exit 1;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0755); err != nil {
		t.Fatalf("writing fake tmux: %v", err)
	}
}

// writeFakeTestBD creates a shell script in dir named "bd" that outputs a
// polecat agent bead JSON. The descState parameter controls what appears in
// the description text (parsed by ParseAgentFields), while
// dbState controls the agent_state database column. updatedAt controls the
// bead's updated_at timestamp for time-bound testing.
func writeFakeTestBD(t *testing.T, dir, descState, dbState, hookBead, updatedAt string) string {
	t.Helper()
	desc := "agent_state: " + descState
	// JSON matches the structure that getAgentBeadInfo expects from bd show --json
	bdJSON := fmt.Sprintf(`[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"%s","hook_bead":"%s","agent_state":"%s","updated_at":"%s"}]`,
		desc, hookBead, dbState, updatedAt)
	// Return agent bead JSON for "show", empty array for "list" (so
	// hasAssignedOpenWork doesn't false-positive on the agent bead).
	script := "#!/bin/sh\nif [ \"$1\" = \"list\" ]; then echo '[]'; exit 0; fi\necho '" + bdJSON + "'\n"
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	return path
}

// writeFakeBDWithHookBead creates a shell script in dir named "bd" that returns
// different JSON based on the bead ID: the agent bead in one state, and the hook
// bead (work bead) in a separate state. Used to test cases where the agent and hook
// beads have independent lifecycles (e.g., agent done/nuked while hook_bead open).
func writeFakeBDWithHookBead(t *testing.T, dir, agentState, hookBeadID, hookBeadStatus, updatedAt string) string {
	t.Helper()
	agentJSON := fmt.Sprintf(`[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"agent_state: %s","hook_bead":"%s","agent_state":"%s","updated_at":"%s"}]`,
		agentState, hookBeadID, agentState, updatedAt)
	hookJSON := fmt.Sprintf(`[{"id":"%s","status":"%s"}]`, hookBeadID, hookBeadStatus)
	script := fmt.Sprintf("#!/bin/sh\n"+
		"if [ \"$1\" = \"list\" ]; then echo '[]'; exit 0; fi\n"+
		"case \"$2\" in\n"+
		"  gt-myr-polecat-mycat) echo '%s';;\n"+
		"  %s) echo '%s';;\n"+
		"  *) echo '[]'; exit 1;;\n"+
		"esac\n", agentJSON, hookBeadID, hookJSON)
	bdPath := filepath.Join(dir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	return bdPath
}

// TestCheckPolecatHealth_SkipsSpawning verifies that checkPolecatHealth does NOT
// attempt to restart a polecat in agent_state=spawning when recently updated.
// This is the regression test for the double-spawn bug (issue #1752): the daemon
// heartbeat fires during the window between bead creation (hook_bead set atomically
// by gt sling) and the actual tmux session launch, causing a second Claude process.
func TestCheckPolecatHealth_SkipsSpawning(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	// Use a recent timestamp so the spawning guard's time-bound is satisfied
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "spawning", "spawning", "gt-xyz", recentTime)

	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "spawning") {
		t.Errorf("expected log to mention 'spawning', got: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("spawning polecat must not trigger CRASH DETECTED, got: %q", got)
	}
}

// TestCheckPolecatHealth_DetectsCrashedPolecat verifies that checkPolecatHealth
// does detect a crash for a polecat in agent_state=working with a dead session.
// This ensures the spawning guard in issue #1752 does not accidentally suppress
// legitimate crash detection for polecats that were running normally.
func TestCheckPolecatHealth_DetectsCrashedPolecat(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "working", "working", "gt-xyz", recentTime)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
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
	binDir := t.TempDir()
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "working", "working", "gt-xyz", recentTime)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
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

// TestCheckPolecatHealth_SpawningGuardExpires verifies that the spawning guard
// has a time-bound: polecats stuck in agent_state=spawning for more than 5 minutes
// are treated as crashed (gt sling may have failed during spawn).
func TestCheckPolecatHealth_SpawningGuardExpires(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	// Use a timestamp >5 minutes ago to expire the spawning guard
	oldTime := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "spawning", "spawning", "gt-xyz", oldTime)

	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "Spawning guard expired") {
		t.Errorf("expected spawning guard to expire for old timestamp, got: %q", got)
	}
	if !strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("expected CRASH DETECTED after spawning guard expires, got: %q", got)
	}
}

// TestCheckPolecatHealth_DescriptionStateOverridesLegacyDBColumn verifies that
// daemon lifecycle reads the description's agent_state first. bd >= 0.62.0 no
// longer has a supported structured agent_state writer, so the description is
// Gastown's active contract and the DB column is legacy fallback only.
func TestCheckPolecatHealth_DescriptionStateOverridesLegacyDBColumn(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	recentTime := time.Now().UTC().Format(time.RFC3339)
	// Description says "spawning" (current Gastown contract) while the legacy
	// structured column still says "working".
	bdPath := writeFakeTestBD(t, binDir, "spawning", "working", "gt-xyz", recentTime)

	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "spawning") {
		t.Errorf("expected log to mention description-backed spawning state, got: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("daemon should honor description state 'spawning' and skip crash detection, got: %q", got)
	}
}

// TestCheckPolecatHealth_SkipsClosedHookBead verifies that checkPolecatHealth
// does NOT fire CRASHED_POLECAT when the hook_bead is already closed.
// This is the regression test for the false-positive spam bug (issue hq-1o7):
// when a polecat completes work normally, the hook_bead gets closed but the
// stale reference remains on the agent bead, causing repeated false alerts.
func TestCheckPolecatHealth_SkipsClosedHookBead(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeBDWithHookBead(t, binDir, "working", "fe-xyz", "closed", recentTime)

	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
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

// TestCheckPolecatHealth_NotifiesWitnessOnCrash verifies that when a polecat
// crash is detected, the daemon sends a notification to the witness via
// `gt mail send` with a CRASHED_POLECAT subject. Restart is deferred to the
// stuck-agent-dog plugin for context-aware recovery.
func TestCheckPolecatHealth_NotifiesWitnessOnCrash(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "working", "working", "gt-xyz", recentTime)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	notes := notifyfake.New()
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notes,
		bdPath:   bdPath,
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "CRASH DETECTED") {
		t.Fatalf("expected CRASH DETECTED, got: %q", got)
	}

	// The witness is told by mail with a CRASHED_POLECAT subject.
	mails := notes.Mails()
	if len(mails) != 1 {
		t.Fatalf("expected one mail to the witness, got: %+v", notes.Calls())
	}
	if mails[0].To != "myr/witness" {
		t.Errorf("mail to %q, want the witness address myr/witness", mails[0].To)
	}
	if !strings.Contains(mails[0].Subject, "CRASHED_POLECAT") {
		t.Errorf("expected CRASHED_POLECAT in mail subject, got: %q", mails[0].Subject)
	}
}

// TestCheckPolecatHealth_SkipsDonePolecat verifies that checkPolecatHealth does
// NOT fire CRASH DETECTED when a polecat has agent_state=done (completed normally)
// even if its hook_bead is still open. This is the race-window regression test for
// bug #2795 part 2: between gt done setting agent_state=done and the hook_bead
// being closed, the daemon heartbeat fires on the dead session + open hook_bead
// combination, causing repeated false CRASHED_POLECAT alerts to the witness.
func TestCheckPolecatHealth_SkipsDonePolecat(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeBDWithHookBead(t, binDir, "done", "gt-xyz", "open", recentTime)

	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "Skipping crash detection") {
		t.Errorf("expected skip log message, got: %q", got)
	}
	if !strings.Contains(got, "agent_state=done") {
		t.Errorf("expected agent_state=done in skip log, got: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("done polecat with open hook_bead must not trigger CRASH DETECTED, got: %q", got)
	}
}

// TestCheckPolecatHealth_SkipsNukedPolecat verifies that checkPolecatHealth does
// NOT fire CRASH DETECTED when a polecat has been nuked (agent_state=nuked) even
// if its hook_bead (work bead) is still open. This is the regression test for
// bug #2795: `gt polecat nuke --force` sets agent_state=nuked on the agent bead
// but leaves the work bead open, causing repeated false RECOVERY_NEEDED alerts
// on every heartbeat cycle.
func TestCheckPolecatHealth_SkipsNukedPolecat(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeBDWithHookBead(t, binDir, "nuked", "gt-xyz", "open", recentTime)

	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
	}

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "Skipping crash detection") {
		t.Errorf("expected skip log message, got: %q", got)
	}
	if !strings.Contains(got, "agent_state=nuked") {
		t.Errorf("expected agent_state=nuked in skip log, got: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Errorf("nuked polecat must not trigger CRASH DETECTED, got: %q", got)
	}
}

// polecatSessionTmux returns a fake tmux holding the polecat session
// "myr-mycat", its pane running paneCommand ("bash" for an idle shell),
// created at created.
func polecatSessionTmux(paneCommand string, created time.Time) *fakeTmux {
	tm := newFakeTmux(newFixedClock())
	tm.addSession("myr-mycat", paneCommand, created)
	return tm
}

// writeFakeTmuxWithAgent creates a shell script that simulates a live tmux session
// with an agent process running. has-session succeeds, display-message returns the
// given paneCommand (e.g., "claude" or "codex") so IsAgentRunning returns true.
func writeFakeTmuxWithAgent(t *testing.T, dir, paneCommand string) {
	t.Helper()
	// Use $* glob matching (not $1) because tmux.run() prepends -u (and
	// optionally -L <socket>) before the subcommand.
	script := fmt.Sprintf("#!/bin/sh\n"+
		"case \"$*\" in\n"+
		"  *has-session*) exit 0;;\n"+
		"  *display-message*) echo '%s';;\n"+
		"  *kill-session*) exit 0;;\n"+
		"  *) exit 1;;\n"+
		"esac\n", paneCommand)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0755); err != nil {
		t.Fatalf("writing fake tmux: %v", err)
	}
}

// writeFakeTmuxIdleSession creates a shell script that simulates a live tmux session
// with NO agent process running (idle shell). has-session succeeds, display-message
// returns "bash" so IsAgentRunning returns false.
func writeFakeTmuxIdleSession(t *testing.T, dir string) {
	t.Helper()
	writeFakeTmuxWithAgent(t, dir, "bash")
}

// writeFakeBDLookupFail creates a "bd" script that fails on "show" (simulating a
// bead infrastructure error) but returns configurable output for "list" queries.
// When hasWork is true, "list" returns an open work bead assigned to "myr/polecats/mycat".
func writeFakeBDLookupFail(t *testing.T, dir string, hasWork bool) string {
	t.Helper()
	listOut := `[]`
	if hasWork {
		listOut = `[{"id":"wh-test-1","status":"open","assignee":"myr/polecats/mycat"}]`
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"list\" ]; then echo '" + listOut + "'; exit 0; fi\n" +
		"# show fails — simulate bead infrastructure degradation\n" +
		"exit 1\n"
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	return path
}

// TestReapIdlePolecat_SkipsWhenBeadLookupFailsButHasWork verifies that reapIdlePolecat
// does NOT kill a polecat when the agent bead lookup fails but hasAssignedOpenWork
// confirms the polecat has an open work bead assigned. This is the regression test
// for the working-bead-lookup-failed kill bug (GH#3342 followup).
func TestReapIdlePolecat_SkipsWhenBeadLookupFailsButHasWork(t *testing.T) {
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	bdPath := writeFakeBDLookupFail(t, binDir, true /* hasWork */)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
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
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	bdPath := writeFakeBDLookupFail(t, binDir, false /* no work */)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
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
	if !strings.Contains(logBuf.String(), "working-bead-lookup-failed") {
		t.Errorf("expected working-bead-lookup-failed reason, got: %q", logBuf.String())
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
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	// Fake bd: agent bead exists but hook_bead is empty (cleared by failed sling)
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "working", "working", "", recentTime)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     polecatSessionTmux("codex", time.Now().Add(-time.Hour)),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
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
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	// Fake bd: agent bead exists but hook_bead is empty
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "working", "working", "", recentTime)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
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
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     polecatSessionTmux("bash", time.Now().Add(-3*time.Minute)),
		notifier: notifyfake.New(),
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
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier: notifyfake.New(),
	}

	sessionName := session.PolecatSessionName(session.PrefixFor("myr"), "mycat")
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

// gt-fcxe9.1: an unanswerable liveness query is UNKNOWN, not dead. With the agent
// bead unreadable and no assigned work, a confirmed-dead agent is reaped at 2x
// the idle threshold; an unknown one must wait for the 3x ceiling like a live
// one. The session's pane runs a shell, so the old error-dropping check read
// the failure as dead and reaped at 2.5x.
func TestReapIdlePolecat_UnknownLivenessIsNotDead(t *testing.T) {
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	bdPath := writeFakeBDLookupFail(t, binDir, false /* no work */)

	townRoot := t.TempDir()
	var logBuf strings.Builder
	tm := polecatSessionTmux("bash", time.Now().Add(-time.Hour))
	tm.setAliveErr("myr-mycat", fmt.Errorf("tmux show-environment: timed out"))
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     tm,
		notifier: notifyfake.New(),
		bdPath:   bdPath,
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

// gt-fcxe9.1: the patrol watchdog passes a dead session outright, so an
// unknown liveness answer must read as alive (judge by receipts) and be logged.
func TestPatrolWatchdogSessionAlive_UnknownReadsAliveAndLogs(t *testing.T) {
	tm := polecatSessionTmux("bash", time.Now().Add(-time.Hour))
	tm.setAliveErr("myr-mycat", fmt.Errorf("tmux show-environment: timed out"))
	var logBuf strings.Builder
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(&logBuf, "", 0), tmux: tm}

	if !d.patrolWatchdogSessionAlive(patrolWatchdogTarget{Session: "myr-mycat"}) {
		t.Fatal("unknown liveness read as dead: the watchdog would pass the patrol without judging it")
	}
	if !strings.Contains(logBuf.String(), "liveness unknown") {
		t.Fatalf("unknown liveness not logged: %q", logBuf.String())
	}
	// A confirmed dead agent (bare shell, no error) still reads as dead.
	tm.setAliveErr("myr-mycat", nil)
	if d.patrolWatchdogSessionAlive(patrolWatchdogTarget{Session: "myr-mycat"}) {
		t.Fatal("a bare shell read as a live agent")
	}
}
