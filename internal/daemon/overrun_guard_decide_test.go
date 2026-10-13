package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDecideOverrun(t *testing.T) {
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
