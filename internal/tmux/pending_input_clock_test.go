package tmux

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The clock exists to answer a question the pane cannot: how long has this
// input been waiting? It must therefore ignore everything the session does and
// measure only the wall time between observations (gt-afa7).
func TestPendingInputClockMeasuresTimeSinceFirstObservation(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	if age := clock.Observe("gt-refinery", true, start); age != 0 {
		t.Errorf("first observation of a pending run = %s, want 0 (it starts the clock)", age)
	}
	for _, delta := range []time.Duration{30 * time.Second, 4 * time.Minute, 17 * time.Minute} {
		age := clock.Observe("gt-refinery", true, start.Add(delta))
		if age != delta {
			t.Errorf("after %s pending, age = %s, want %s", delta, age, delta)
		}
	}
}

// A session that consumes its input and later receives more starts a new wait.
// Inheriting the old age would report a fresh nudge as a long stall.
func TestPendingInputClockRestartsAfterInputIsConsumed(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", true, start)
	if age := clock.Observe("gt-refinery", true, start.Add(10*time.Minute)); age != 10*time.Minute {
		t.Fatalf("age before the drain = %s, want 10m", age)
	}

	// The agent picked the input up: the run is over.
	if age := clock.Observe("gt-refinery", false, start.Add(11*time.Minute)); age != 0 {
		t.Errorf("age on a non-pending observation = %s, want 0", age)
	}

	// New input, new wait.
	fresh := start.Add(12 * time.Minute)
	if age := clock.Observe("gt-refinery", true, fresh); age != 0 {
		t.Errorf("first observation of the new run = %s, want 0 (it must not inherit the old run)", age)
	}
	if age := clock.Observe("gt-refinery", true, fresh.Add(time.Minute)); age != time.Minute {
		t.Errorf("age = %s, want 1m", age)
	}
}

// Reset is what lets a caller act on the input without the very next probe
// reading the age of input that has already been handled.
func TestPendingInputClockResetForgetsTheRun(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", true, start)
	if age := clock.Observe("gt-refinery", true, start.Add(6*time.Minute)); age != 6*time.Minute {
		t.Fatalf("age = %s, want 6m", age)
	}

	clock.Reset("gt-refinery")
	if age := clock.Observe("gt-refinery", true, start.Add(6*time.Minute+time.Second)); age != 0 {
		t.Errorf("age after Reset = %s, want 0", age)
	}
}

// Separate sessions must not share a stamp — the daemon probes witness and
// refinery for every rig on the same heartbeat.
func TestPendingInputClockTracksSessionsSeparately(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", true, start)
	if age := clock.Observe("gt-witness", true, start.Add(3*time.Minute)); age != 0 {
		t.Errorf("a session observed for the first time = %s, want 0", age)
	}
	if age := clock.Observe("gt-refinery", true, start.Add(3*time.Minute)); age != 3*time.Minute {
		t.Errorf("age for the session that was already pending = %s, want 3m", age)
	}
}

// A clock with nowhere to persist is worse than none: the two detectors that
// share it would each measure their own wait, and neither would ever reach the
// threshold on a four-minute heartbeat. Callers that cannot supply a state
// directory get a no-op instead.
func TestPendingInputClockDisabledWithoutStateDir(t *testing.T) {
	t.Parallel()
	for name, clock := range map[string]*PendingInputClock{
		"nil clock": NewPendingInputClock(""),
		"blank":     NewPendingInputClock("   "),
	} {
		start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)
		if age := clock.Observe("gt-refinery", true, start.Add(time.Hour)); age != 0 {
			t.Errorf("%s: age = %s, want 0", name, age)
		}
		clock.Reset("gt-refinery") // must not panic
	}
}

// A stamp file that has been truncated or corrupted by a concurrent writer must
// read as "no run in progress" rather than pinning an absurd age or failing the
// probe. The age is an input to a stall verdict, so a bad read may only ever
// make the detector more conservative.
func TestPendingInputClockIgnoresUnreadableStamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clock := NewPendingInputClock(dir)
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	if err := os.MkdirAll(filepath.Join(dir, composerPendingSubdir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stamp := filepath.Join(dir, composerPendingSubdir, "gt-refinery")
	for name, content := range map[string]string{
		"garbage":    "not-a-timestamp",
		"truncated":  "",
		"zero":       "0",
		"negative":   "-1",
		"whitespace": "  \n ",
	} {
		if err := os.WriteFile(stamp, []byte(content), 0o644); err != nil {
			t.Fatalf("write stamp: %v", err)
		}
		if age := clock.Observe("gt-refinery", true, start); age != 0 {
			t.Errorf("%s stamp: age = %s, want 0", name, age)
		}
	}
}

// Session names reach the clock straight from tmux, and one containing a path
// separator must not write outside the state directory.
func TestPendingInputClockKeepsStampsInsideItsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clock := NewPendingInputClock(dir)
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("../../escape", true, start)
	entries, err := os.ReadDir(filepath.Join(dir, composerPendingSubdir))
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("state dir holds %d entries, want 1: %v", len(entries), entries)
	}
	if entries[0].IsDir() {
		t.Errorf("stamp %q is a directory", entries[0].Name())
	}

	// The stamp must still work, under its sanitized name.
	if age := clock.Observe("../../escape", true, start.Add(2*time.Minute)); age != 2*time.Minute {
		t.Errorf("age = %s, want 2m", age)
	}
}
