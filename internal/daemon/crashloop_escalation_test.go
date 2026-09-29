package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

func TestShouldEscalateCrashLoop(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{})

	// Not in crash loop: never escalate.
	if rt.ShouldEscalateCrashLoop("deacon", time.Hour) {
		t.Fatal("escalated for agent not in crash loop")
	}

	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-5 * time.Minute),
	}

	// First check while in crash loop: escalate.
	if !rt.ShouldEscalateCrashLoop("deacon", time.Hour) {
		t.Fatal("did not escalate on first crash-loop check")
	}
	// Immediately after: suppressed (interval not elapsed).
	if rt.ShouldEscalateCrashLoop("deacon", time.Hour) {
		t.Fatal("re-escalated before interval elapsed")
	}

	// Backdate the last escalation past the interval: re-escalate.
	rt.state.Agents["deacon"].LastCrashLoopEscalation = time.Now().Add(-2 * time.Hour)
	if !rt.ShouldEscalateCrashLoop("deacon", time.Hour) {
		t.Fatal("did not re-escalate after interval elapsed")
	}
}

func TestShouldEscalateCrashLoop_ResetOnClear(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{})
	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-5 * time.Minute),
	}

	if !rt.ShouldEscalateCrashLoop("deacon", time.Hour) {
		t.Fatal("did not escalate on first crash-loop check")
	}

	// Operator clears the backoff; a fresh crash loop later must escalate
	// immediately, not wait out the old interval.
	rt.ClearCrashLoop("deacon")
	rt.state.Agents["deacon"].CrashLoopSince = time.Now()
	if !rt.ShouldEscalateCrashLoop("deacon", time.Hour) {
		t.Fatal("did not escalate for a fresh crash loop after clear")
	}
}

// Regression test for gt-e7h: crash-loop skip for a core agent must escalate
// to the mayor instead of silently logging.
func TestEscalateCrashLoopSkip_CoreAgent(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "daemon"), 0o755); err != nil {
		t.Fatalf("mkdir daemon: %v", err)
	}
	rec := notifyfake.New()

	rt := NewRestartTracker(townRoot, RestartTrackerConfig{})
	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-5 * time.Minute),
	}

	d := &Daemon{
		config:         &Config{TownRoot: townRoot},
		logger:         log.New(io.Discard, "", 0),
		restartTracker: rt,
		notifier:       rec,
	}

	d.escalateCrashLoopSkip("deacon", "test reason")

	got := rec.Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations = %d, want 1: %+v", len(got), got)
	}
	e := got[0].Escalation
	if e.Severity != "HIGH" || e.Fingerprint != "crash-loop:deacon" {
		t.Errorf("escalation = %+v, want HIGH under crash-loop:deacon", e)
	}
	if !strings.Contains(e.Description, "gt daemon clear-backoff deacon") {
		t.Errorf("escalation missing clear-backoff command: %q", e.Description)
	}

	// Second skip within the interval: no duplicate escalation.
	d.escalateCrashLoopSkip("deacon", "test reason")
	if got := rec.Escalations(); len(got) != 1 {
		t.Fatalf("escalations after repeat skip = %d, want 1", len(got))
	}
}

// Silent skip is acceptable for polecats/dogs (gt-e7h).
func TestEscalateCrashLoopSkip_NonCoreAgentSilent(t *testing.T) {
	townRoot := t.TempDir()
	rec := notifyfake.New()

	rt := NewRestartTracker(townRoot, RestartTrackerConfig{})
	rt.state.Agents["polecat-onyx"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-5 * time.Minute),
	}

	d := &Daemon{
		config:         &Config{TownRoot: townRoot},
		logger:         log.New(io.Discard, "", 0),
		restartTracker: rt,
		notifier:       rec,
	}

	d.escalateCrashLoopSkip("polecat-onyx", "test reason")

	if got := rec.Calls(); len(got) != 0 {
		t.Fatalf("sends = %+v, want none for a non-core agent", got)
	}
}
