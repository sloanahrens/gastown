package patrolstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func capTimeout(at time.Time, sid string) *WaitOutcome {
	return &WaitOutcome{Reason: "timeout", Timeout: 5 * time.Minute, BackoffMax: 5 * time.Minute, AtCap: true, SessionID: sid, At: at}
}

func TestWaitOutcomeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if o, err := ReadWaitOutcome(dir); o != nil || err != nil {
		t.Fatalf("missing file = (%v, %v), want (nil, nil)", o, err)
	}
	want := *capTimeout(t0, "s1")
	want.IdleCycles, want.EffortLevel = 6, "abbreviated"
	if err := WriteWaitOutcome(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWaitOutcome(dir)
	if err != nil || got == nil {
		t.Fatalf("read = (%v, %v)", got, err)
	}
	want.Version = cycleFileVersion
	if *got != want {
		t.Fatalf("round trip = %+v, want %+v", *got, want)
	}
}

func TestReadWaitOutcomeRejectsCorruptAndUnknownVersion(t *testing.T) {
	for name, body := range map[string]string{
		"corrupt":         "{not json",
		"unknown version": `{"version": 99, "reason": "timeout", "at_cap": true}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeState(t, filepath.Join(dir, runtimeDirName), waitOutcomeFile, body)
			if o, err := ReadWaitOutcome(dir); o != nil || err == nil {
				t.Fatalf("read = (%v, %v), want (nil, error)", o, err)
			}
		})
	}
}

func TestCycleStateRoundTripAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	if s, err := LoadCycleState(dir); err != nil || s != (CycleState{}) {
		t.Fatalf("missing = (%+v, %v)", s, err)
	}
	in := CycleState{SessionID: "s1", Cycles: 4, LastWaitAt: t0, UpdatedAt: t0}
	if err := SaveCycleState(dir, in); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCycleState(dir)
	in.Version = cycleFileVersion
	if err != nil || got != in {
		t.Fatalf("round trip = (%+v, %v), want %+v", got, err, in)
	}
	if err := os.WriteFile(CycleStatePath(dir), []byte("garbage"), 0644); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadCycleState(dir); err == nil || s != (CycleState{}) {
		t.Fatalf("corrupt = (%+v, %v), want zero state and an error", s, err)
	}
}

func TestAdvanceCycleCountsPerSessionAndResetsOnFreshSession(t *testing.T) {
	var s CycleState
	for i := 1; i <= 3; i++ {
		step := AdvanceCycle(s, "s1", nil, t0)
		if step.State.Cycles != i || step.FreshSession != (i == 1) {
			t.Fatalf("cycle %d: state=%+v fresh=%v", i, step.State, step.FreshSession)
		}
		s = step.State
	}
	step := AdvanceCycle(s, "s2", nil, t0)
	if !step.FreshSession || step.State.Cycles != 1 || step.State.SessionID != "s2" {
		t.Fatalf("new session: %+v fresh=%v, want the count restarted at 1", step.State, step.FreshSession)
	}
}

func TestAdvanceCycleConsumesEachWaitOnce(t *testing.T) {
	w := capTimeout(t0, "s1")
	first := AdvanceCycle(CycleState{SessionID: "s1", Cycles: 2}, "s1", w, t0)
	if first.Wait != w || !first.State.LastWaitAt.Equal(t0) {
		t.Fatalf("first report did not take the wait: %+v", first)
	}
	second := AdvanceCycle(first.State, "s1", w, t0.Add(time.Minute))
	if second.Wait != nil {
		t.Fatal("a second report reused the same wait; the agent skipped await-signal")
	}
	newer := capTimeout(t0.Add(6*time.Minute), "s1")
	if third := AdvanceCycle(second.State, "s1", newer, t0); third.Wait != newer {
		t.Fatal("a newer wait was not attributed")
	}
}

func TestAdvanceCycleIgnoresAnotherSessionsWait(t *testing.T) {
	// The predecessor's wait, never consumed (it died before reporting).
	w := capTimeout(t0, "old")
	step := AdvanceCycle(CycleState{SessionID: "old", Cycles: 5}, "new", w, t0)
	if step.Wait != nil || !step.FreshSession || step.State.Cycles != 1 {
		t.Fatalf("fresh session took its predecessor's wait: %+v", step)
	}
	// And a consumed watermark survives the session change.
	consumed := CycleState{SessionID: "old", Cycles: 5, LastWaitAt: t0}
	if s := AdvanceCycle(consumed, "new", capTimeout(t0, ""), t0); s.Wait != nil || !s.State.LastWaitAt.Equal(t0) {
		t.Fatalf("watermark lost across a session change: %+v", s)
	}
}

// TestCycleBoundaryTruthTable is the approved boundary (Sloan, claude-8w7):
// respawn when the wait timed out at the idle cap and the session has run at
// least 3 cycles, or at 8 cycles regardless.
func TestCycleBoundaryTruthTable(t *testing.T) {
	signal := &WaitOutcome{Reason: "signal", At: t0}
	belowCap := &WaitOutcome{Reason: "timeout", Timeout: 2 * time.Minute, BackoffMax: 5 * time.Minute, At: t0}
	atCap := capTimeout(t0, "")
	noCapConfigured := &WaitOutcome{Reason: "timeout", Timeout: time.Minute, At: t0}

	cases := []struct {
		name    string
		cycles  int
		wait    *WaitOutcome
		respawn bool
		reason  string
	}{
		{"cap timeout, 1 cycle", 1, atCap, false, "needs 3 cycles"},
		{"cap timeout, 2 cycles", 2, atCap, false, "needs 3 cycles"},
		{"cap timeout, 3 cycles", 3, atCap, true, "idle cap after 3 cycles"},
		{"cap timeout, 5 cycles", 5, atCap, true, "idle cap after 5 cycles"},
		{"non-cap timeout, 5 cycles", 5, belowCap, false, "below the idle cap"},
		{"no cap configured, 5 cycles", 5, noCapConfigured, false, "below the idle cap"},
		{"signal wake, 5 cycles", 5, signal, false, "woke on a signal"},
		{"no wait recorded, 5 cycles", 5, nil, false, "no await-signal outcome"},
		{"backstop: signal at 8", 8, signal, true, "backstop"},
		{"backstop: nothing at 8", 8, nil, true, "backstop"},
		{"backstop: non-cap timeout at 9", 9, belowCap, true, "backstop"},
		{"signal at 7", 7, signal, false, "woke on a signal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := CycleBoundary(tc.cycles, tc.wait, 3, 8)
			if got != tc.respawn || !strings.Contains(reason, tc.reason) {
				t.Fatalf("CycleBoundary = (%v, %q), want (%v, ~%q)", got, reason, tc.respawn, tc.reason)
			}
		})
	}
}

func TestCycleBoundaryBackstopDisabled(t *testing.T) {
	if got, _ := CycleBoundary(50, nil, 3, 0); got {
		t.Fatal("backstop fired with maxCycles 0")
	}
}
