package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

func writeFakeGt(t *testing.T, dir, logPath string) {
	t.Helper()
	script := `#!/usr/bin/env bash
printf "%s\n" "$*" >> "` + logPath + `"
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "gt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}
}

func readEscalations(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read gt log: %v", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// Regression test for gt-e7h: crash-loop skip for a core agent must escalate
// to the mayor instead of silently logging.
func TestEscalateCrashLoopSkip_CoreAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake gt requires bash")
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "daemon"), 0o755); err != nil {
		t.Fatalf("mkdir daemon: %v", err)
	}
	fakeBinDir := t.TempDir()
	gtLog := filepath.Join(t.TempDir(), "gt.log")
	writeFakeGt(t, fakeBinDir, gtLog)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := NewRestartTracker(townRoot, RestartTrackerConfig{})
	rt.state.Agents["deacon"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-5 * time.Minute),
	}

	d := &Daemon{
		config:         &Config{TownRoot: townRoot},
		logger:         log.New(io.Discard, "", 0),
		restartTracker: rt,
	}

	d.escalateCrashLoopSkip("deacon", "test reason")

	lines := readEscalations(t, gtLog)
	if len(lines) != 1 {
		t.Fatalf("gt invocations = %d, want 1 escalation; log: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "escalate") || !strings.Contains(lines[0], "HIGH") {
		t.Errorf("escalation missing severity: %q", lines[0])
	}
	if !strings.Contains(lines[0], "gt daemon clear-backoff deacon") {
		t.Errorf("escalation missing clear-backoff command: %q", lines[0])
	}

	// Second skip within the interval: no duplicate escalation.
	d.escalateCrashLoopSkip("deacon", "test reason")
	if lines := readEscalations(t, gtLog); len(lines) != 1 {
		t.Fatalf("gt invocations after repeat skip = %d, want 1", len(lines))
	}
}

// Silent skip is acceptable for polecats/dogs (gt-e7h).
func TestEscalateCrashLoopSkip_NonCoreAgentSilent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake gt requires bash")
	}
	townRoot := t.TempDir()
	fakeBinDir := t.TempDir()
	gtLog := filepath.Join(t.TempDir(), "gt.log")
	writeFakeGt(t, fakeBinDir, gtLog)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := NewRestartTracker(townRoot, RestartTrackerConfig{})
	rt.state.Agents["polecat-onyx"] = &AgentRestartInfo{
		CrashLoopSince: time.Now().Add(-5 * time.Minute),
	}

	d := &Daemon{
		config:         &Config{TownRoot: townRoot},
		logger:         log.New(io.Discard, "", 0),
		restartTracker: rt,
	}

	d.escalateCrashLoopSkip("polecat-onyx", "test reason")

	if lines := readEscalations(t, gtLog); len(lines) != 0 {
		t.Fatalf("gt invocations = %d, want 0 for non-core agent; log: %q", len(lines), lines)
	}
}
