package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDecideOverrun(t *testing.T) {
	t.Parallel()
	slung := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	const nudgeAt, stopAt = 12 * time.Minute, 20 * time.Minute

	tests := []struct {
		name    string
		slungAt time.Time
		elapsed time.Duration // Now = slung + elapsed
		nudged  bool
		want    overrunAction
	}{
		{"just slung", slung, 0, false, overrunNone},
		{"one second short of nudge", slung, 11*time.Minute + 59*time.Second, false, overrunNone},
		{"nudge threshold, not nudged", slung, 12 * time.Minute, false, overrunNudge},
		{"nudge threshold, already nudged", slung, 12 * time.Minute, true, overrunNone},
		{"between nudge and stop, already nudged", slung, 15 * time.Minute, true, overrunNone},
		{"one second short of stop, not nudged", slung, 19*time.Minute + 59*time.Second, false, overrunNudge},
		{"one second short of stop, already nudged", slung, 19*time.Minute + 59*time.Second, true, overrunNone},
		{"stop threshold, never nudged", slung, 20 * time.Minute, false, overrunStop},
		{"stop threshold, nudged", slung, 20 * time.Minute, true, overrunStop},
		{"far past stop", slung, 45 * time.Minute, true, overrunStop},
		{"zero SlungAt", time.Time{}, 45 * time.Minute, false, overrunNone},
		{"SlungAt in the future", slung, -time.Minute, false, overrunNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := overrunInput{
				Now:     slung.Add(tt.elapsed),
				SlungAt: tt.slungAt,
				Nudged:  tt.nudged,
				NudgeAt: nudgeAt,
				StopAt:  stopAt,
			}
			if got := decideOverrun(in); got != tt.want {
				t.Errorf("decideOverrun(elapsed %v, nudged %v) = %d, want %d", tt.elapsed, tt.nudged, got, tt.want)
			}
		})
	}
}

func TestOverrunStateRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	t.Run("missing file is the zero state", func(t *testing.T) {
		got, err := readOverrunState(town, "gt-none")
		if err != nil {
			t.Fatalf("readOverrunState: %v", err)
		}
		if got != (overrunState{}) {
			t.Errorf("got %+v, want zero state", got)
		}
	})

	t.Run("write then read equals", func(t *testing.T) {
		want := overrunState{
			Bead:     "gt-abc12",
			NudgedAt: time.Date(2026, 10, 13, 12, 12, 0, 0, time.UTC),
		}
		if err := writeOverrunState(town, want); err != nil {
			t.Fatalf("writeOverrunState: %v", err)
		}
		got, err := readOverrunState(town, want.Bead)
		if err != nil {
			t.Fatalf("readOverrunState: %v", err)
		}
		if !got.NudgedAt.Equal(want.NudgedAt) || got.Bead != want.Bead || !got.StoppedAt.IsZero() {
			t.Errorf("got %+v, want %+v", got, want)
		}

		want.StoppedAt = time.Date(2026, 10, 13, 12, 20, 0, 0, time.UTC)
		if err := writeOverrunState(town, want); err != nil {
			t.Fatalf("second writeOverrunState: %v", err)
		}
		got, err = readOverrunState(town, want.Bead)
		if err != nil {
			t.Fatalf("second readOverrunState: %v", err)
		}
		if !got.StoppedAt.Equal(want.StoppedAt) {
			t.Errorf("StoppedAt = %v, want %v", got.StoppedAt, want.StoppedAt)
		}
	})

	t.Run("zero times are omitted and no temp file is left", func(t *testing.T) {
		if err := writeOverrunState(town, overrunState{Bead: "gt-zero1"}); err != nil {
			t.Fatalf("writeOverrunState: %v", err)
		}
		dir := filepath.Join(town, ".runtime", "overrun-guard")
		data, err := os.ReadFile(filepath.Join(dir, "gt-zero1.json"))
		if err != nil {
			t.Fatalf("read state file: %v", err)
		}
		if want := `{"bead":"gt-zero1"}`; string(data) != want {
			t.Errorf("file = %s, want %s", data, want)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir: %v", err)
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".json" {
				t.Errorf("leftover non-state file %s", e.Name())
			}
		}
	})

	t.Run("corrupt file is an error", func(t *testing.T) {
		dir := filepath.Join(town, ".runtime", "overrun-guard")
		if err := os.WriteFile(filepath.Join(dir, "gt-bad01.json"), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readOverrunState(town, "gt-bad01"); err == nil {
			t.Error("want an error for a corrupt state file")
		}
	})

	t.Run("bead id cannot leave the state directory", func(t *testing.T) {
		for _, bead := range []string{"", "../escape", "a/b", `a\b`} {
			if _, err := readOverrunState(town, bead); err == nil {
				t.Errorf("readOverrunState(%q): want an error", bead)
			}
			if err := writeOverrunState(town, overrunState{Bead: bead}); err == nil {
				t.Errorf("writeOverrunState(%q): want an error", bead)
			}
		}
	})
}

// Gate time is subtracted from the wall time the rule measures: a seat inside
// its own gate runs past both thresholds without a nudge or a stop, and one
// whose non-gate time passes the stop threshold is stopped even inside its gate
// (gt-vfwuc).
func TestDecideOverrun_GateSpent(t *testing.T) {
	t.Parallel()
	slung := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	const nudgeAt, stopAt = 12 * time.Minute, 20 * time.Minute

	tests := []struct {
		name      string
		elapsed   time.Duration // wall time since the sling
		gateSpent time.Duration
		nudged    bool
		want      overrunAction
	}{
		{"25 minutes entirely inside the gate", 25 * time.Minute, 25 * time.Minute, false, overrunNone},
		{"25 minutes with 15 of them in the gate", 25 * time.Minute, 15 * time.Minute, false, overrunNone},
		{"25 minutes with 12 of them in the gate", 25 * time.Minute, 12 * time.Minute, false, overrunNudge},
		{"25 minutes with 4 of them in the gate", 25 * time.Minute, 4 * time.Minute, false, overrunStop},
		{"gate time past the wall time leaves nothing to judge", 25 * time.Minute, 40 * time.Minute, false, overrunNone},
		{"no gate time is the plain rule", 20 * time.Minute, 0, true, overrunStop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := overrunInput{
				Now: slung.Add(tt.elapsed), SlungAt: slung, Nudged: tt.nudged,
				NudgeAt: nudgeAt, StopAt: stopAt, GateSpent: tt.gateSpent,
			}
			if got := decideOverrun(in); got != tt.want {
				t.Errorf("decideOverrun(wall %v, gate %v, nudged %v) = %d, want %d",
					tt.elapsed, tt.gateSpent, tt.nudged, got, tt.want)
			}
		})
	}
}

// withGate charges the guard's own ticks: a stretch is banked across intervals
// whose both ends were seen inside the gate, and never past the last tick that
// saw one (gt-vfwuc).
func TestOverrunStateWithGate(t *testing.T) {
	t.Parallel()
	slung := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	const maxCharge, maxTotal = 2 * time.Minute, time.Hour

	tests := []struct {
		name      string
		ticks     []bool // one per minute after the sling: was the seat in its gate
		wantSpent time.Duration
	}{
		{"never in a gate", []bool{false, false, false}, 0},
		{"the first tick finds the gate", []bool{true}, 0},
		{"two ticks inside", []bool{true, true}, time.Minute},
		{"inside then out", []bool{true, true, false}, time.Minute},
		{"two stretches", []bool{true, true, false, true, true}, 2 * time.Minute},
		{"out of the gate for the rest of the run", []bool{true, true, false, false}, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var st overrunState
			var spent time.Duration
			for i, inGate := range tt.ticks {
				st, spent = st.withGate(slung, slung.Add(time.Duration(i+1)*time.Minute), inGate, maxCharge, maxTotal)
			}
			if spent != tt.wantSpent || st.GateSpent != tt.wantSpent {
				t.Errorf("gate time after ticks %v = %v (state %v), want %v", tt.ticks, spent, st.GateSpent, tt.wantSpent)
			}
		})
	}
}

// A tick the guard skipped is a gap: the seat may have left its gate and
// re-entered inside it unwatched, so the gap past the last tick that saw the
// gate open counts as the seat's own time, and the clock keeps counting
// (gt-vfwuc).
func TestOverrunStateWithGate_GapIsNotCharged(t *testing.T) {
	t.Parallel()
	slung := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	const maxCharge, maxTotal = 2 * time.Minute, time.Hour

	var st overrunState
	st, _ = st.withGate(slung, slung.Add(time.Minute), true, maxCharge, maxTotal) // seen in the gate
	st, spent := st.withGate(slung, slung.Add(21*time.Minute), true, maxCharge, maxTotal)

	if spent != maxCharge {
		t.Errorf("gate time across a 20 minute gap = %v, want %v (the charge is capped, the rest counts as the seat's time)", spent, maxCharge)
	}
}
