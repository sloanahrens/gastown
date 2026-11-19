package done

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

func gatePkg(name string, d time.Duration, timed bool) land.PackageResult {
	return land.PackageResult{Package: name, Elapsed: d, Timed: timed}
}

func gateStep(name string, d time.Duration, pkgs ...land.PackageResult) land.StepResult {
	return land.StepResult{Name: name, Elapsed: d, Packages: pkgs}
}

// TestLocalGateNote is the Gate text runDoneLocalGate leaves on the READY TO
// LAND note: verdict first, wall, slot wait, then the slowest timed packages
// (gt-7o5cql).
func TestLocalGateNote(t *testing.T) {
	t.Parallel()
	presubmit := func(d time.Duration, pkgs ...land.PackageResult) land.GateResult {
		return land.GateResult{Passed: true, Steps: []land.StepResult{gateStep("presubmit", d, pkgs...)}}
	}
	tests := []struct {
		name     string
		res      land.GateResult
		slotWait time.Duration
		want     string
	}{
		{"pass names wall, slot wait and the slowest package first", presubmit(16*time.Second, gatePkg("a", 8*time.Second, true), gatePkg("b", 5*time.Second, true), gatePkg("c", time.Second, true)), 3 * time.Second, "pass presubmit 16s, slot wait 3s, slowest: a 8s, b 5s, c 1s"},
		{"a failing gate says fail", land.GateResult{Steps: []land.StepResult{gateStep("presubmit", 4*time.Second, gatePkg("a", 4*time.Second, true))}}, 0, "fail presubmit 4s, slot wait 0s, slowest: a 4s"},
		{"six packages list the five slowest", presubmit(21*time.Second, gatePkg("a", time.Second, true), gatePkg("b", 2*time.Second, true), gatePkg("c", 3*time.Second, true), gatePkg("d", 4*time.Second, true), gatePkg("e", 5*time.Second, true), gatePkg("f", 6*time.Second, true)), 0, "pass presubmit 21s, slot wait 0s, slowest: f 6s, e 5s, d 4s, c 3s, b 2s"},
		{"an untimed package is not a cost", presubmit(2*time.Second, gatePkg("cached", 0, false), gatePkg("slow", 2*time.Second, true)), 0, "pass presubmit 2s, slot wait 0s, slowest: slow 2s"},
		{"several steps are named gate and their walls sum", land.GateResult{Passed: true, Steps: []land.StepResult{gateStep("lint", 2*time.Second), gateStep("test", 6*time.Second)}}, 0, "pass gate 8s, slot wait 0s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := localGateNote(tc.res, tc.slotWait); got != tc.want {
				t.Errorf("localGateNote() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLocalGateNoteReachesTheReadyNote: runDoneLocalGate's text is the note's Gate line.
func TestLocalGateNoteReachesTheReadyNote(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.gate.result = land.GateResult{Passed: true, Steps: []land.StepResult{{Name: "presubmit", Elapsed: 16 * time.Second, Packages: []land.PackageResult{gatePkg("a", 8*time.Second, true), gatePkg("b", 5*time.Second, true), gatePkg("c", time.Second, true)}}}}
	// A fixed 3s slot wait on the run's clock; the fake gate waits for nothing.
	t0, calls := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), 0
	h.r.now = func() time.Time { calls++; return t0.Add(time.Duration(calls-1) * 3 * time.Second) }
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	w, ok := land.ParseReadyNote(h.source(t).Notes)
	if !ok {
		t.Fatalf("the source bead has no READY TO LAND note: %q", h.source(t).Notes)
	}
	if want := "pass presubmit 16s, slot wait 3s, slowest: a 8s, b 5s, c 1s"; w.Gate != want {
		t.Errorf("Gate = %q, want %q", w.Gate, want)
	}
}

// TestRunDoneLocalGateNotesAPreVerifiedSkip: --pre-verified runs no gate and notes it.
func TestRunDoneLocalGateNotesAPreVerifiedSkip(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.r.opts.preVerified = true
	if err := runDoneLocalGate(h.r, "abc1234"); err != nil {
		t.Fatalf("runDoneLocalGate with --pre-verified: %v", err)
	}
	if len(h.gate.heads) != 0 || h.r.gateNote != localGatePreVerifiedNote {
		t.Errorf("gate ran %d time(s) and noted %q; want no run and %q", len(h.gate.heads), h.r.gateNote, localGatePreVerifiedNote)
	}
}
