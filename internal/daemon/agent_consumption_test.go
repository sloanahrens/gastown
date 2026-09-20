package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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

// writeFakeTmuxPane installs a tmux shim on PATH that answers capture-pane from
// a sequence of panes (repeating the last one) and reports a frozen
// window_activity. It returns the shim's invocation log so a test can assert
// which keystrokes were sent.
func writeFakeTmuxPane(t *testing.T, panes []string) string {
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
	b.WriteString(`if [ "$sub" = "display-message" ]; then printf '%s' '` + frozenActivity + `'; exit 0; fi` + "\n")
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
