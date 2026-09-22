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
//
// The observations below are spaced like the daemon's heartbeat, and the
// session id and progress digest are held fixed, which is the case a run is
// earned in.
const (
	testSessionID  = "$3"
	testProgressID = "a1b2c3d4"
)

func TestPendingInputClockMeasuresTimeSinceFirstObservation(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	if wait := clock.Observe("gt-refinery", testSessionID, testProgressID, start); wait.Waiting != 0 {
		t.Errorf("first observation of a pending run = %s, want 0 (it starts the clock)", wait.Waiting)
	}
	for _, delta := range []time.Duration{30 * time.Second, 4 * time.Minute, 17 * time.Minute} {
		wait := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(delta))
		if wait.Waiting != delta {
			t.Errorf("after %s pending, waiting = %s, want %s", delta, wait.Waiting, delta)
		}
	}
}

// A run may not be measured from two samples a caller happened to take. The
// gt-afa7 defect is exactly that: input seen at 15:40 and again at 15:50 reads
// as ten continuous minutes even if the agent consumed a turn's worth of work
// in between (gt-afa7).
func TestPendingInputClockRequiresConsecutiveSamples(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	first := clock.Observe("gt-refinery", testSessionID, testProgressID, start)
	if first.Samples != 1 || first.Continuous {
		t.Errorf("first observation = %+v, want 1 sample and not continuous", first)
	}

	second := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(2*time.Minute))
	if second.Samples != 2 || second.Continuous {
		t.Errorf("second observation = %+v, want 2 samples and not continuous yet", second)
	}

	third := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(4*time.Minute))
	if third.Samples != 3 || !third.Continuous {
		t.Errorf("third observation = %+v, want 3 samples spanning 4m and continuous", third)
	}
	if third.Waiting != 4*time.Minute {
		t.Errorf("waiting = %s, want 4m", third.Waiting)
	}
}

// The minimum span is separate from the sample count so a burst of re-probes
// cannot earn a run. The witness re-checks a composer three times 750ms after
// submitting; those samples must not add up to a threshold's worth of evidence.
func TestPendingInputClockRejectsABurstOfSamples(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	var last PendingWait
	for i := 0; i < 5; i++ {
		last = clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(time.Duration(i)*time.Second))
	}
	if last.Continuous {
		t.Errorf("five samples one second apart = %+v, want not continuous (they span %s)",
			last, pendingInputMinSpan)
	}
}

// A session that consumes its input and later receives more starts a new wait.
// Inheriting the old age would report a fresh nudge as a long stall.
func TestPendingInputClockRestartsAfterInputIsConsumed(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", testSessionID, testProgressID, start)
	if wait := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(10*time.Minute)); wait.Waiting != 10*time.Minute {
		t.Fatalf("waiting before the drain = %s, want 10m", wait.Waiting)
	}

	// The agent picked the input up: the run is over.
	clock.Reset("gt-refinery")

	// New input, new wait.
	fresh := start.Add(12 * time.Minute)
	if wait := clock.Observe("gt-refinery", testSessionID, testProgressID, fresh); wait.Waiting != 0 || wait.Samples != 1 {
		t.Errorf("first observation of the new run = %+v, want a fresh run that inherits no age", wait)
	}
}

// The whole point of the progress digest: an agent that did any work during the
// window restarts the clock. This is what keeps the age from acting on a
// working or idle-await session that merely happens to be carrying a queued
// nudge — the gt-hkhu false positive (gt-afa7).
func TestPendingInputClockRestartsWhenThePaneShowsProgress(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", testSessionID, "before", start)
	clock.Observe("gt-refinery", testSessionID, "before", start.Add(4*time.Minute))
	earned := clock.Observe("gt-refinery", testSessionID, "before", start.Add(8*time.Minute))
	if !earned.Continuous {
		t.Fatalf("run over a still pane = %+v, want continuous", earned)
	}

	// The agent produced output above the input box. The wait it was carrying
	// was not unattended, so its age must not survive into the new state.
	after := clock.Observe("gt-refinery", testSessionID, "after", start.Add(12*time.Minute))
	if after.Waiting != 0 || after.Samples != 1 || after.Continuous {
		t.Errorf("observation after pane progress = %+v, want a fresh run", after)
	}
}

// A stamp must not outlive the session that wrote it. Worktree and rig names are
// reused, so a refinery restarted under the same name would otherwise inherit an
// age of hours and be flushed on its first probe, before it had a chance to
// consume the input (gt-afa7).
func TestPendingInputClockRestartsWhenTheSessionIsRestarted(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", "$3", testProgressID, start)
	if wait := clock.Observe("gt-refinery", "$3", testProgressID, start.Add(20*time.Minute)); wait.Waiting != 20*time.Minute {
		t.Fatalf("waiting = %s, want 20m", wait.Waiting)
	}

	// Same name, same pane, new session.
	restarted := clock.Observe("gt-refinery", "$4", testProgressID, start.Add(20*time.Minute+time.Second))
	if restarted.Waiting != 0 || restarted.Samples != 1 {
		t.Errorf("observation in a restarted session = %+v, want a fresh run that inherits no age", restarted)
	}
}

// An identity or a digest the caller could not measure is not a run we may date.
// Recording one anyway would let a stamp accumulate across sessions nobody can
// tell apart.
func TestPendingInputClockRefusesUnmeasurableObservations(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", testSessionID, testProgressID, start)
	for name, args := range map[string][2]string{
		"no session id":      {"", testProgressID},
		"no progress digest": {testSessionID, ""},
	} {
		wait := clock.Observe("gt-refinery", args[0], args[1], start.Add(9*time.Minute))
		if wait.Waiting != 0 || wait.Samples != 0 {
			t.Errorf("%s: observation = %+v, want no run", name, wait)
		}
		// And it must have discarded the stamp, not merely reported zero.
		if next := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(9*time.Minute)); next.Waiting != 0 {
			t.Errorf("%s: next observation = %+v, want a fresh run", name, next)
		}
	}
}

// Reset is what lets a caller act on the input without the very next probe
// reading the age of input that has already been handled.
func TestPendingInputClockResetForgetsTheRun(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", testSessionID, testProgressID, start)
	if wait := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(6*time.Minute)); wait.Waiting != 6*time.Minute {
		t.Fatalf("waiting = %s, want 6m", wait.Waiting)
	}

	clock.Reset("gt-refinery")
	if wait := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(6*time.Minute+time.Second)); wait.Waiting != 0 || wait.Samples != 1 {
		t.Errorf("observation after Reset = %+v, want a fresh run", wait)
	}
}

// Separate sessions must not share a stamp — the daemon probes witness and
// refinery for every rig on the same heartbeat.
func TestPendingInputClockTracksSessionsSeparately(t *testing.T) {
	t.Parallel()
	clock := NewPendingInputClock(t.TempDir())
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	clock.Observe("gt-refinery", testSessionID, testProgressID, start)
	if wait := clock.Observe("gt-witness", testSessionID, testProgressID, start.Add(3*time.Minute)); wait.Waiting != 0 {
		t.Errorf("a session observed for the first time = %s, want 0", wait.Waiting)
	}
	if wait := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(3*time.Minute)); wait.Waiting != 3*time.Minute {
		t.Errorf("waiting for the session that was already pending = %s, want 3m", wait.Waiting)
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
		if wait := clock.Observe("gt-refinery", testSessionID, testProgressID, start.Add(time.Hour)); wait.Waiting != 0 {
			t.Errorf("%s: waiting = %s, want 0", name, wait.Waiting)
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
		"garbage":      "not-a-stamp",
		"truncated":    `{"first_unix":`,
		"empty object": `{}`,
		"zero first":   `{"first_unix":0,"samples":3,"progress":"p"}`,
		"zero samples": `{"first_unix":1700000000,"samples":0,"progress":"p"}`,
		"legacy plain": "1700000000",
		"empty":        "",
		"whitespace":   "  \n ",
	} {
		if err := os.WriteFile(stamp, []byte(content), 0o644); err != nil {
			t.Fatalf("write stamp: %v", err)
		}
		if wait := clock.Observe("gt-refinery", testSessionID, testProgressID, start); wait.Waiting != 0 {
			t.Errorf("%s stamp: waiting = %s, want 0", name, wait.Waiting)
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

	clock.Observe("../../escape", testSessionID, testProgressID, start)
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
	if wait := clock.Observe("../../escape", testSessionID, testProgressID, start.Add(2*time.Minute)); wait.Waiting != 2*time.Minute {
		t.Errorf("waiting = %s, want 2m", wait.Waiting)
	}
}

// A session name that sanitizes to a traversal ("..", ".") must not reach the
// filesystem at all — a file named ".." is not a file. A name that merely
// contains dots is not a traversal: it is sanitized into a contained filename,
// which is what the replacer is for.
func TestPendingInputClockRejectsTraversalNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clock := NewPendingInputClock(dir)
	start := time.Date(2026, 9, 22, 15, 40, 56, 0, time.UTC)

	for _, session := range []string{"", "..", ".", "...", "...."} {
		if p := clock.path(session); p != "" {
			t.Errorf("path(%q) = %q, want no stamp path", session, p)
		}
		if wait := clock.Observe(session, testSessionID, testProgressID, start); wait.Waiting != 0 {
			t.Errorf("session %q: waiting = %s, want 0", session, wait.Waiting)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(dir, composerPendingSubdir)); err == nil {
		for _, e := range entries {
			t.Errorf("state dir holds %q, want nothing written for a traversal name", e.Name())
		}
	}
}
