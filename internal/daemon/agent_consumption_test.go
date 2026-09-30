package daemon

import (
	"bytes"
	"errors"
	"io"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/tmux"
)

// The consumption probe (gt-eigw) acts on the verdict *tmux.Tmux's composer
// detector returns; the detector itself, over real pane captures, is tested in
// internal/tmux. These tests script the verdicts on a fakeTmux and pin what the
// daemon does with each.

// wedgedStall is the gt-eigw verdict: an idle prompt with nudges stranded in
// Claude Code's input queue and no pane output for longer than the threshold.
var wedgedStall = tmux.ComposerStall{
	State:      tmux.ComposerPending,
	Queued:     true,
	Inactivity: time.Hour,
	FrozenFor:  5 * time.Minute,
	Stalled:    true,
	Evidence:   "queued input behind the send-now footer, no pane output for 1h0m0s",
}

// recoveredStall is the same session after the flush: the composer is clean.
var recoveredStall = tmux.ComposerStall{State: tmux.ComposerClean, Evidence: "composer empty"}

// newConsumptionTestDaemon builds the smallest Daemon the probe touches: a
// fake tmux holding the agent's session, answering the probes with stalls in
// order (repeating the last), a town root for the thresholds, and a
// discarded logger.
func newConsumptionTestDaemon(t *testing.T, agent runningAgent, stalls ...tmux.ComposerStall) (*Daemon, *fakeTmux) {
	t.Helper()
	tm := newFakeTmux(newFixedClock())
	tm.addSession(agent.Session, "claude", time.Time{})
	tm.setStalls(agent.Session, stalls...)
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		tmux:   tm,
		logger: log.New(io.Discard, "", 0),
	}, tm
}

// newConsumptionTestDaemonWithLog is newConsumptionTestDaemon with the
// daemon's log captured, so a test can assert what a probe said.
func newConsumptionTestDaemonWithLog(t *testing.T, agent runningAgent, stalls ...tmux.ComposerStall) (*Daemon, *fakeTmux, *bytes.Buffer) {
	t.Helper()
	d, tm := newConsumptionTestDaemon(t, agent, stalls...)
	buf := &bytes.Buffer{}
	d.logger = log.New(buf, "", 0)
	return d, tm, buf
}

// submits returns the recorded SubmitPendingInput calls.
func submits(tm *fakeTmux) []string {
	var out []string
	for _, c := range tm.daemonCalls() {
		if strings.HasPrefix(c, "SubmitPendingInput ") {
			out = append(out, c)
		}
	}
	return out
}

// withNoConsumptionRecheckDelay removes the settle time between the flush and
// the re-probe. Package state, so callers must not run in parallel.
func withNoConsumptionRecheckDelay(t *testing.T) {
	t.Helper()
	original := consumptionRecheckDelay
	consumptionRecheckDelay = 0
	t.Cleanup(func() { consumptionRecheckDelay = original })
}

func testRefineryAgent() runningAgent {
	return runningAgent{Role: "refinery", Rig: "beads", Session: "gt-beads-refinery"}
}

func testWitnessAgent() runningAgent {
	return runningAgent{Role: "witness", Rig: "beads", Session: "gt-beads-witness"}
}

// A wedged session that clears once its pending input is submitted must be
// flushed and then left alone: no escalation, and the flush must use the
// keystroke that matches the pending state (ctrl+x ctrl+s for queued input).
func TestProbeInputConsumptionRecoversWedgedSession(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	d, tm := newConsumptionTestDaemon(t, agent, wedgedStall, recoveredStall)

	var escalations []string
	d.probeInputConsumption(agent, func(source, message string) {
		escalations = append(escalations, source+": "+message)
	})

	if len(escalations) != 0 {
		t.Errorf("recovered session escalated: %v", escalations)
	}
	if got, want := submits(tm), []string{"SubmitPendingInput queued " + agent.Session}; !slices.Equal(got, want) {
		t.Errorf("submits = %v, want %v (queued input is flushed with ctrl+x ctrl+s)", got, want)
	}
}

// Input typed into the composer (not queued) is flushed with the typed-input
// keystroke: the probe passes the verdict's Queued through.
func TestProbeInputConsumptionSubmitsTypedInputAsTyped(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	typed := wedgedStall
	typed.Queued = false
	d, tm := newConsumptionTestDaemon(t, agent, typed, recoveredStall)

	d.probeInputConsumption(agent, nil)

	if got, want := submits(tm), []string{"SubmitPendingInput typed " + agent.Session}; !slices.Equal(got, want) {
		t.Errorf("submits = %v, want %v", got, want)
	}
}

// A session that is still holding the input after the flush is the case the
// patrol cannot fix with a keystroke. It must escalate, and it must escalate
// exactly once per interval — the heartbeat runs every few minutes.
func TestProbeInputConsumptionEscalatesOncePerInterval(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	d, _ := newConsumptionTestDaemon(t, agent, wedgedStall)

	var escalations []string
	escalate := func(source, message string) {
		escalations = append(escalations, source+": "+message)
	}

	d.probeInputConsumption(agent, escalate)
	d.probeInputConsumption(agent, escalate)
	d.probeInputConsumption(agent, escalate)

	if len(escalations) != 1 {
		t.Fatalf("escalated %d times across three heartbeats, want 1: %v", len(escalations), escalations)
	}
	for _, want := range []string{"gt-beads-refinery", "consuming no input", "gt refinery restart beads", wedgedStall.Evidence} {
		if !strings.Contains(escalations[0], want) {
			t.Errorf("escalation %q does not mention %q", escalations[0], want)
		}
	}
}

// The daemon must never kill a session from this path. Attempt 1 of gt-eigw was
// blocked for killing every healthy session ~5 minutes after startup, so pin
// that the session survives an unrecoverable stall.
func TestProbeInputConsumptionNeverKills(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	d, tm := newConsumptionTestDaemon(t, agent, wedgedStall)

	d.probeInputConsumption(agent, func(string, string) {})

	if has, _ := tm.HasSession(agent.Session); !has {
		t.Errorf("the probe killed %s", agent.Session)
	}
}

// A session that is not stalled must not be touched at all — no keystrokes, no
// escalation. This is the guard against the serial-killer failure mode for
// agents that are legitimately idle: every other role writes a heartbeat, and
// an idle refinery legitimately produces no pane output between MRs.
func TestProbeInputConsumptionIgnoresHealthySession(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	d, tm := newConsumptionTestDaemon(t, agent, recoveredStall)

	var escalations []string
	d.probeInputConsumption(agent, func(source, message string) {
		escalations = append(escalations, source)
	})

	if len(escalations) != 0 {
		t.Errorf("healthy session escalated: %v", escalations)
	}
	if got := submits(tm); len(got) != 0 {
		t.Errorf("healthy session received keystrokes: %v", got)
	}
}

// An unreadable pane says nothing about the session, so it must not escalate.
func TestProbeInputConsumptionSilentWhenPaneUnreadable(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testWitnessAgent()
	d, tm := newConsumptionTestDaemon(t, agent)
	tm.setStallErr(agent.Session, errors.New("tmux capture-pane: no server running"))

	var escalations []string
	d.probeInputConsumption(agent, func(source, message string) {
		escalations = append(escalations, source)
	})

	if len(escalations) != 0 {
		t.Errorf("unreadable pane escalated: %v", escalations)
	}
	if got := submits(tm); len(got) != 0 {
		t.Errorf("unreadable pane received keystrokes: %v", got)
	}
}

// Each role restarts through its own rig-scoped command. The escalation has to
// name the right one: 'gt session restart' only handles polecats, so pointing a
// witness or refinery operator at it would send them to a command that fails.
func TestRunningAgentRestartHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		agent runningAgent
		want  string
	}{
		{runningAgent{Role: "witness", Rig: "gastown"}, "gt witness restart gastown"},
		{runningAgent{Role: "refinery", Rig: "beads"}, "gt refinery restart beads"},
	}
	for _, tt := range tests {
		if got := tt.agent.restartHint(); got != tt.want {
			t.Errorf("%s/%s restartHint() = %q, want %q", tt.agent.Role, tt.agent.Rig, got, tt.want)
		}
	}
}

// The heartbeat's witness probe must target the witness session the daemon
// itself maintains for a rig — a probe aimed at the wrong session name reports
// a healthy rig while the real session stays wedged. (The refinery probe went
// away with the refinery role, gt-v4ssj.6.)
//
// The assertion is on the suffix rather than on a literal name: the rig prefix
// comes from the session registry, which a test binary does not populate, so a
// hardcoded prefix would test the test.
func TestProbeRunningRolesTargetTheRigSessions(t *testing.T) {
	t.Parallel()
	tm := newFakeTmux(newFixedClock())
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		tmux:     tm,
		logger:   log.New(io.Discard, "", 0),
		notifier: notifyfake.New(),
	}

	d.probeRunningWitness("gastown")

	var targets []string
	for _, call := range tm.daemonCalls() {
		if session, ok := strings.CutPrefix(call, "DetectComposerStallTracked "); ok {
			targets = append(targets, session)
		}
	}

	if len(targets) != 1 {
		t.Fatalf("probed %v, want exactly one witness session", targets)
	}
	if !strings.HasSuffix(targets[0], "-witness") {
		t.Fatalf("probed %v, want a witness session", targets)
	}
}

func TestConsumptionEscalatorRateLimits(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	now := time.Now()
	e := &consumptionEscalator{}

	if !e.shouldEscalate("gt-beads-refinery", now) {
		t.Fatal("first call was rate-limited")
	}
	if e.shouldEscalate("gt-beads-refinery", now.Add(time.Minute)) {
		t.Error("second call within the interval was allowed")
	}
	if !e.shouldEscalate("gt-beads-refinery", now.Add(consumptionEscalationInterval+time.Second)) {
		t.Error("call after the interval was suppressed")
	}
	// A different session has its own budget.
	if !e.shouldEscalate("gt-beads-witness", now) {
		t.Error("a second session was rate-limited by the first session's escalation")
	}
}

// --- Pending-input age (gt-afa7) ------------------------------------------

// The regression for the 2026-09-22 stall was that input which had outlived
// the threshold went unnoticed while typing refreshed #{window_activity}. The
// detector can date the input only from the pending clock the daemon hands it,
// so every probe — the first and the re-probe after the flush — must get the
// town's clock, and it must be the same clock each time.
func TestProbeInputConsumptionHandsTheDetectorThePendingClock(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	d, tm := newConsumptionTestDaemon(t, agent, wedgedStall, recoveredStall)

	d.probeInputConsumption(agent, nil)
	d.probeInputConsumption(agent, nil)

	clocks := tm.stallClocks()
	if len(clocks) != 3 {
		t.Fatalf("stall probes = %d, want 3 (probe, re-probe after the flush, next heartbeat)", len(clocks))
	}
	for i, c := range clocks {
		if c == nil {
			t.Fatalf("probe %d got no pending clock: input waiting behind a busy pane could never be dated", i)
		}
		if c != clocks[0] {
			t.Fatalf("probe %d got a different pending clock: a run's age would restart every heartbeat", i)
		}
	}
}

// A probe that finds input waiting but not yet past the threshold must say so.
// This is the branch that stayed silent through the three heartbeats on
// 2026-09-22, leaving a 17-minute stall indistinguishable in the log from a
// healthy session — the "left silent" half of the defect (gt-afa7).
func TestProbeInputConsumptionReportsInputWaitingBelowTheThreshold(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	waiting := tmux.ComposerStall{
		State:          tmux.ComposerPending,
		Queued:         true,
		PendingFor:     time.Minute,
		PendingSamples: 1,
		FrozenFor:      5 * time.Minute,
		Evidence:       "queued input behind the send-now footer",
	}
	d, tm, buf := newConsumptionTestDaemonWithLog(t, agent, waiting)

	var escalations []string
	d.probeInputConsumption(agent, func(source, message string) {
		escalations = append(escalations, source)
	})

	if len(escalations) != 0 {
		t.Errorf("input below the threshold was escalated: %v", escalations)
	}
	if got := submits(tm); len(got) != 0 {
		t.Errorf("input below the threshold was submitted: %v", got)
	}

	said := buf.String()
	if !strings.Contains(said, "holding unsubmitted input") {
		t.Errorf("the probe said nothing about the input it saw waiting; log: %q", said)
	}
	for _, want := range []string{"gt-beads-refinery", "still watching", waiting.Evidence} {
		if !strings.Contains(said, want) {
			t.Errorf("log %q does not mention %q", said, want)
		}
	}
}

// A pane the probe can read but not classify is a detection failure, not a
// clean bill of health. It must be reported, and it must not be escalated: the
// daemon cannot act on what it cannot see, and escalating would fire forever on
// any agent whose TUI the probe cannot parse.
func TestProbeInputConsumptionReportsUnclassifiablePane(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	d, tm, buf := newConsumptionTestDaemonWithLog(t, agent)
	tm.setStallErr(agent.Session, tmux.ErrComposerUnobservable)

	var escalations []string
	d.probeInputConsumption(agent, func(source, message string) {
		escalations = append(escalations, source)
	})

	if len(escalations) != 0 {
		t.Errorf("an unclassifiable pane was escalated: %v", escalations)
	}
	if got := submits(tm); len(got) != 0 {
		t.Errorf("an unclassifiable pane received keystrokes: %v", got)
	}
	if !strings.Contains(buf.String(), "could not classify") {
		t.Errorf("the probe silently accepted an unclassifiable pane; log: %q", buf.String())
	}
}

// A pane the probe cannot classify stays that way across heartbeats — a modal,
// or an agent TUI with no prompt prefix. Reporting it every four minutes per
// agent buries the lines that matter, so it is reported on a cooldown: the
// first occurrence always logs, repeats do not (gt-afa7).
func TestProbeInputConsumptionRateLimitsTheUnclassifiableLog(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	d, tm, buf := newConsumptionTestDaemonWithLog(t, agent)
	tm.setStallErr(agent.Session, tmux.ErrComposerUnobservable)

	for i := 0; i < 3; i++ {
		d.probeInputConsumption(agent, nil)
	}

	if got := strings.Count(buf.String(), "could not classify"); got != 1 {
		t.Errorf("the unclassifiable pane was reported %d times across three probes, want 1\nlog: %q",
			got, buf.String())
	}
}

// The daemon's stall line has to say how long the pane was quiet: it logs the
// detector's evidence, which carries the timing on every branch, and says what
// it did about it (gt-afa7).
func TestProbeInputConsumptionLogsHowLongThePaneWasSilent(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	agent := testRefineryAgent()
	d, _, buf := newConsumptionTestDaemonWithLog(t, agent, wedgedStall, recoveredStall)

	d.probeInputConsumption(agent, nil)

	said := buf.String()
	if !strings.Contains(said, wedgedStall.Evidence) {
		t.Errorf("the stall log line does not carry the detector's evidence %q; log: %q", wedgedStall.Evidence, said)
	}
	if !strings.Contains(said, "submitting its pending input") {
		t.Errorf("the stall log line does not say what was done about it; log: %q", said)
	}
}

// The two log lines answer different questions — "this session is wedged" and
// "I could not read this session" — so suppressing one must not suppress the
// other.
func TestConsumptionEscalatorRateLimitsTheUnclassifiableLogSeparately(t *testing.T) {
	t.Parallel()
	e := &consumptionEscalator{}
	now := time.Now()

	if !e.shouldLogUnclassifiable("gt-beads-refinery", now) {
		t.Error("the first unclassifiable observation for a session was suppressed")
	}
	if e.shouldLogUnclassifiable("gt-beads-refinery", now.Add(time.Minute)) {
		t.Error("a repeat within the interval was logged")
	}
	if !e.shouldLogUnclassifiable("gt-beads-refinery", now.Add(consumptionEscalationInterval+time.Second)) {
		t.Error("a repeat after the interval was suppressed")
	}
	// A separate session has its own budget.
	if !e.shouldLogUnclassifiable("gt-beads-witness", now) {
		t.Error("a second session was rate-limited by the first session's line")
	}

	// Escalating the same session must not silence its next log line.
	e2 := &consumptionEscalator{}
	if !e2.shouldLogUnclassifiable("gt-beads-refinery", now) {
		t.Fatal("the first unclassifiable observation was suppressed")
	}
	e2.shouldEscalate("gt-beads-refinery", now)
	if e2.shouldLogUnclassifiable("gt-beads-refinery", now.Add(time.Minute)) {
		t.Error("an escalation suppressed a later unclassifiable line; the budgets must be separate")
	}
}
