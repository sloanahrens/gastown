package daemon

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Pane fixtures for the daemon's input-consumption probe (gt-eigw). These are
// trimmed copies of the live captures pinned in
// internal/tmux/composer_stall_test.go; this package cannot reach those
// constants.
const (
	// daemonWedgedPane is the gt-eigw signature: an idle prompt with nudges
	// stranded in Claude Code's input queue.
	daemonWedgedPane = "⏺ Waiting on the review.\n" +
		"\n" +
		"────────────────────────────────────────\n" +
		"❯ \n" +
		"  Press up to edit queued messages · ctrl+x ctrl+s to send now\n" +
		"────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage"

	// daemonRecoveredPane is the same session after the flush: the queue is
	// empty and the agent is working again.
	daemonRecoveredPane = "⏺ Picked up the queued instruction.\n" +
		"\n" +
		"────────────────────────────────────────\n" +
		"❯\n" +
		"────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage"
)

// frozenActivity is a window_activity timestamp far older than any threshold:
// the session has produced no pane output since 2023-11-14.
const frozenActivity = "1700000000"

// testSessionID is the tmux session id the shim reports. Stable across probes,
// which is what lets a test date a pending run: a real session id changes only
// when the session is killed and recreated.
const testSessionID = "$7"

// writeFakeTmuxPane installs a tmux shim on PATH that answers capture-pane from
// a sequence of panes (repeating the last one) and reports a frozen
// window_activity. It returns the shim's invocation log so a test can assert
// which keystrokes were sent.
func writeFakeTmuxPane(t *testing.T, panes []string) string {
	t.Helper()
	return writeFakeTmuxPaneWithActivity(t, panes, frozenActivity)
}

// writeFakeTmuxPaneWithActivity is writeFakeTmuxPane with the session's
// window_activity chosen by the caller. "@now" answers with the current Unix
// timestamp, which is what a pane being repainted right now reports — the
// condition the silence clock cannot see through (gt-afa7).
func writeFakeTmuxPaneWithActivity(t *testing.T, panes []string, activity string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}
	if len(panes) == 0 {
		t.Fatal("writeFakeTmuxPane needs at least one pane")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "tmux.log")
	countPath := filepath.Join(binDir, "capture.count")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$*" >> "` + logPath + `"` + "\n")
	b.WriteString(`for a in "$@"; do case "$a" in` + "\n")
	b.WriteString("\tcapture-pane|display-message|has-session|send-keys|show-environment) sub=$a; break;;\n")
	b.WriteString("\tesac; done\n")
	// The probe reads two tmux variables: window_activity for the silence clock
	// and session_id for the identity that dates the pending-input clock. They
	// are answered separately so a pane being repainted right now can still
	// belong to a session whose id is stable (gt-afa7).
	b.WriteString(`fmt=''` + "\n")
	b.WriteString(`for a in "$@"; do case "$a" in '#'*) fmt=$a;; esac; done` + "\n")
	b.WriteString(`if [ "$sub" = "display-message" ]; then` + "\n")
	b.WriteString(`  if [ "$fmt" = '#{session_id}' ]; then printf '%s' '` + testSessionID + `'; exit 0; fi` + "\n")
	if activity == "@now" {
		b.WriteString(`  date +%s; exit 0;` + "\n")
	} else {
		b.WriteString(`  printf '%s' '` + activity + `'; exit 0;` + "\n")
	}
	b.WriteString("fi\n")
	b.WriteString(`if [ "$sub" = "has-session" ]; then exit 0; fi` + "\n")
	b.WriteString(`if [ "$sub" = "capture-pane" ]; then` + "\n")
	b.WriteString(`  n=$(cat "` + countPath + `" 2>/dev/null || echo 0)` + "\n")
	b.WriteString(`  echo $((n+1)) > "` + countPath + `"` + "\n")
	b.WriteString("  case \"$n\" in\n")
	for i, pane := range panes {
		if i == len(panes)-1 {
			break
		}
		b.WriteString("\t" + strconv.Itoa(i) + ") printf '%s' '" + pane + "'; exit 0;;\n")
	}
	b.WriteString("\t*) printf '%s' '" + panes[len(panes)-1] + "'; exit 0;;\n")
	b.WriteString("  esac\n")
	b.WriteString("fi\n")
	b.WriteString("exit 0\n")

	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// newConsumptionTestDaemon builds the smallest Daemon the probe touches: a
// tmux handle, a town root for the thresholds, a discarded logger, and a
// temp-dir config.
func newConsumptionTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		tmux:   tmux.NewTmuxWithSocket("gt-test-daemon-consumption"),
		logger: log.New(io.Discard, "", 0),
	}
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
	logPath := writeFakeTmuxPane(t, []string{daemonWedgedPane, daemonRecoveredPane})
	d := newConsumptionTestDaemon(t)

	var escalations []string
	d.probeInputConsumption(testRefineryAgent(), func(source, message string) {
		escalations = append(escalations, source+": "+message)
	})

	if len(escalations) != 0 {
		t.Errorf("recovered session escalated: %v", escalations)
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if !strings.Contains(string(logged), "C-x C-s") {
		t.Errorf("queued input was not flushed with ctrl+x ctrl+s; tmux log: %q", logged)
	}
}

// A session that is still holding the input after the flush is the case the
// patrol cannot fix with a keystroke. It must escalate, and it must escalate
// exactly once per interval — the heartbeat runs every few minutes.
func TestProbeInputConsumptionEscalatesOncePerInterval(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	writeFakeTmuxPane(t, []string{daemonWedgedPane})
	d := newConsumptionTestDaemon(t)

	var escalations []string
	escalate := func(source, message string) {
		escalations = append(escalations, source+": "+message)
	}

	d.probeInputConsumption(testRefineryAgent(), escalate)
	d.probeInputConsumption(testRefineryAgent(), escalate)
	d.probeInputConsumption(testRefineryAgent(), escalate)

	if len(escalations) != 1 {
		t.Fatalf("escalated %d times across three heartbeats, want 1: %v", len(escalations), escalations)
	}
	for _, want := range []string{"gt-beads-refinery", "consuming no input", "gt refinery restart beads"} {
		if !strings.Contains(escalations[0], want) {
			t.Errorf("escalation %q does not mention %q", escalations[0], want)
		}
	}
}

// The daemon must never kill a session from this path. Attempt 1 of gt-eigw was
// blocked for killing every healthy session ~5 minutes after startup, so pin
// the absence of the kill verb rather than trusting the call sites.
func TestProbeInputConsumptionNeverKills(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	logPath := writeFakeTmuxPane(t, []string{daemonWedgedPane})
	d := newConsumptionTestDaemon(t)

	d.probeInputConsumption(testRefineryAgent(), func(string, string) {})

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	for _, verb := range []string{"kill-session", "kill-server", "kill-pane", "respawn-pane", "respawn-window"} {
		if strings.Contains(string(logged), verb) {
			t.Errorf("probe invoked %s; tmux log: %q", verb, logged)
		}
	}
}

// A session that is not stalled must not be touched at all — no keystrokes, no
// escalation. This is the guard against the serial-killer failure mode for
// agents that are legitimately idle: every other role writes a heartbeat, and
// an idle refinery legitimately produces no pane output between MRs.
func TestProbeInputConsumptionIgnoresHealthySession(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	// A frozen session whose composer is clean: no input is waiting, so
	// nothing is stranded.
	logPath := writeFakeTmuxPane(t, []string{"⏺ Queue empty. Awaiting work.\n❯\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"})
	d := newConsumptionTestDaemon(t)

	var escalations []string
	d.probeInputConsumption(testRefineryAgent(), func(source, message string) {
		escalations = append(escalations, source)
	})

	if len(escalations) != 0 {
		t.Errorf("healthy session escalated: %v", escalations)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if strings.Contains(string(logged), "send-keys") {
		t.Errorf("healthy session received keystrokes; tmux log: %q", logged)
	}
}

// An unreadable pane says nothing about the session, so it must not escalate.
func TestProbeInputConsumptionSilentWhenPaneUnreadable(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	t.Setenv("PATH", t.TempDir()) // no tmux at all
	d := newConsumptionTestDaemon(t)

	var escalations []string
	d.probeInputConsumption(testWitnessAgent(), func(source, message string) {
		escalations = append(escalations, source)
	})

	if len(escalations) != 0 {
		t.Errorf("unreadable pane escalated: %v", escalations)
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

// The two functions the heartbeat calls must probe the pair of role sessions
// the daemon itself maintains for a rig — a probe aimed at the wrong session
// name reports a healthy rig while the real session stays wedged.
//
// The assertion is on the two targets relative to each other rather than on a
// literal name: the rig prefix comes from the session registry, which a test
// binary does not populate, so a hardcoded prefix would test the test.
func TestProbeRunningRolesTargetTheRigSessions(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	logPath := writeFakeTmuxPane(t, []string{daemonWedgedPane})
	d := newConsumptionTestDaemon(t)

	d.probeRunningWitness("gastown")
	d.probeRunningRefinery("gastown")

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}

	const capture = "capture-pane -p -e -t "
	var targets []string
	for _, line := range strings.Split(string(logged), "\n") {
		idx := strings.Index(line, capture)
		if idx < 0 {
			continue
		}
		rest := line[idx+len(capture):]
		if end := strings.Index(rest, " "); end >= 0 {
			rest = rest[:end]
		}
		if len(targets) == 0 || targets[len(targets)-1] != rest {
			targets = append(targets, rest)
		}
	}

	if len(targets) != 2 {
		t.Fatalf("probed %v, want exactly one witness session and one refinery session", targets)
	}
	if !strings.HasSuffix(targets[0], "-witness") || !strings.HasSuffix(targets[1], "-refinery") {
		t.Fatalf("probed %v, want a witness session then a refinery session", targets)
	}
	if prefix := strings.TrimSuffix(targets[0], "-witness"); prefix != strings.TrimSuffix(targets[1], "-refinery") {
		t.Errorf("the two probes used different rig prefixes: %q vs %q", targets[0], targets[1])
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

// newConsumptionTestDaemonWithLog is newConsumptionTestDaemon with the daemon's
// log captured, so a test can assert what a probe that found nothing to act on
// actually said.
func newConsumptionTestDaemonWithLog(t *testing.T) (*Daemon, *bytes.Buffer) {
	t.Helper()
	d := newConsumptionTestDaemon(t)
	buf := &bytes.Buffer{}
	d.logger = log.New(buf, "", 0)
	return d, buf
}

// ageComposerPending backdates the daemon's pending-input clock for session, as
// an earlier heartbeat or a patrol cycle would have. pane is the pane the shim
// is serving, because the run's progress digest has to be the one the probe
// will compute over that same pane — a stamp carrying any other digest reads as
// a run that ended, which is a different test.
func ageComposerPending(t *testing.T, d *Daemon, session, pane string, age time.Duration) {
	t.Helper()
	clock := tmux.NewPendingInputClock(constants.TownRuntimePath(d.config.TownRoot))
	if clock == nil {
		t.Fatal("pending clock is disabled for the test town root")
	}
	progress := tmux.PaneProgressSignature(pane, tmux.DefaultReadyPromptPrefix)
	if progress == "" {
		t.Fatal("the fixture pane has no content above its input box, so no run can be dated on it")
	}

	// Several observations spanning most of the age, so the run the probe reads
	// is continuous rather than one that has just restarted.
	now := time.Now()
	for i := 0; i < pendingSeededSamples; i++ {
		at := now.Add(-age + time.Duration(i)*age/pendingSeededSamples)
		clock.Observe(session, testSessionID, progress, at)
	}
}

// pendingSeededSamples matches pendingInputMinSamples in tmux: enough
// observations for a run to be continuous. Spelled here rather than imported
// because the daemon must not depend on the clock's internals.
const pendingSeededSamples = 3

// The regression test for the 2026-09-22 stall. The gastown refinery was idle
// at the prompt with deacon HEALTH_CHECK nudges queued behind the send-now
// footer, and the daemon's probe ran three times across 17 minutes without
// acting: every nudge typed into the composer had refreshed the
// #{window_activity} clock the verdict depended on. Input that has outlived the
// threshold is a stall however busy the pane's byte stream looks.
func TestProbeInputConsumptionSubmitsInputThatOutlivedTheClock(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	logPath := writeFakeTmuxPaneWithActivity(t, []string{daemonWedgedPane, daemonRecoveredPane}, "@now")
	d := newConsumptionTestDaemon(t)
	ageComposerPending(t, d, testRefineryAgent().Session, daemonWedgedPane, 6*time.Minute)

	var escalations []string
	d.probeInputConsumption(testRefineryAgent(), func(source, message string) {
		escalations = append(escalations, source+": "+message)
	})

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if !strings.Contains(string(logged), "C-x C-s") {
		t.Errorf("queued input that outlived the clock was not flushed; tmux log: %q", logged)
	}
	if len(escalations) != 0 {
		t.Errorf("a session that recovered on the flush was escalated: %v", escalations)
	}
}

// A probe that finds input waiting but not yet past the threshold must say so.
// This is the branch that stayed silent through the three heartbeats on
// 2026-09-22, leaving a 17-minute stall indistinguishable in the log from a
// healthy session — the "left silent" half of the defect (gt-afa7).
func TestProbeInputConsumptionReportsInputWaitingBelowTheThreshold(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	logPath := writeFakeTmuxPaneWithActivity(t, []string{daemonWedgedPane}, "@now")
	d, buf := newConsumptionTestDaemonWithLog(t)

	// The clock records this observation; nothing has aged yet.
	var escalations []string
	d.probeInputConsumption(testRefineryAgent(), func(source, message string) {
		escalations = append(escalations, source)
	})

	if len(escalations) != 0 {
		t.Errorf("input below the threshold was escalated: %v", escalations)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if strings.Contains(string(logged), "send-keys") {
		t.Errorf("input below the threshold was submitted; tmux log: %q", logged)
	}

	said := buf.String()
	if !strings.Contains(said, "holding unsubmitted input") {
		t.Errorf("the probe said nothing about the input it saw waiting; log: %q", said)
	}
	for _, want := range []string{"gt-beads-refinery", "still watching"} {
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
	logPath := writeFakeTmuxPaneWithActivity(t,
		[]string{"⏺ Build finished.\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"}, "@now")
	d, buf := newConsumptionTestDaemonWithLog(t)

	var escalations []string
	d.probeInputConsumption(testRefineryAgent(), func(source, message string) {
		escalations = append(escalations, source)
	})

	if len(escalations) != 0 {
		t.Errorf("an unclassifiable pane was escalated: %v", escalations)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if strings.Contains(string(logged), "send-keys") {
		t.Errorf("an unclassifiable pane received keystrokes; tmux log: %q", logged)
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
	writeFakeTmuxPaneWithActivity(t,
		[]string{"⏺ Build finished.\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"}, "@now")
	d, buf := newConsumptionTestDaemonWithLog(t)

	for i := 0; i < 3; i++ {
		d.probeInputConsumption(testRefineryAgent(), nil)
	}

	if got := strings.Count(buf.String(), "could not classify"); got != 1 {
		t.Errorf("the unclassifiable pane was reported %d times across three probes, want 1\nlog: %q",
			got, buf.String())
	}
}

// The daemon's stall line has to say how long the pane was quiet. It lost that
// when the detector's silence-only branch stopped reporting a duration: the line
// said what was seen but not for how long, which is the number an operator reads
// to decide whether to restart a refinery by hand (gt-afa7).
func TestProbeInputConsumptionLogsHowLongThePaneWasSilent(t *testing.T) {
	withNoConsumptionRecheckDelay(t)
	// A frozen session holding queued input: the silence clock is what trips.
	writeFakeTmuxPane(t, []string{daemonWedgedPane, daemonRecoveredPane})
	d, buf := newConsumptionTestDaemonWithLog(t)

	d.probeInputConsumption(testRefineryAgent(), nil)

	said := buf.String()
	if !strings.Contains(said, "no pane output for") {
		t.Errorf("the stall log line carries no silence duration; log: %q", said)
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
