package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/imagedrift"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// imageDriftTown is four clean candidates on a town with one sandbox seat that
// takes all four.
func imageDriftTown() *fakeSpecTown {
	f := newFakeSpecTown(
		cleanSpec("gt-a", 2, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 2, "2026-09-29T10:01:00Z"),
		cleanSpec("gt-c", 2, "2026-09-29T10:02:00Z"),
		cleanSpec("gt-d", 2, "2026-09-29T10:03:00Z"),
	)
	f.seats = []specdispatch.Seat{{Agent: "sbx-flash", Cap: 4}}
	return f
}

// driftEnv is the town's env with a drift marker in a directory of the test's
// own, one sling at a time, and a Sling that counts its calls and refuses the
// first n of them for the lagging image.
func driftEnv(t *testing.T, f *fakeSpecTown, refuseFirst int) (specDispatchEnv, *int) {
	t.Helper()
	env := f.env()
	env.PerTick = 4
	env.MaxInFlight = 1
	env.Drift = imagedrift.StoreIn(filepath.Join(t.TempDir(), imagedrift.FileName))
	calls := new(int)
	inner := env.Sling
	env.Sling = func(c specCandidate, resume string, seat specdispatch.SeatChoice) (string, error) {
		*calls++
		if *calls <= refuseFirst {
			return "", fmt.Errorf("starting polecat session: %w", session.ErrImageDrift)
		}
		return inner(c, resume, seat)
	}
	return env, calls
}

func notTriedLines(r specDispatchReport) []string {
	var out []string
	for _, e := range r.Skipped {
		if strings.Contains(e.Line, "not tried") {
			out = append(out, e.Line)
		}
	}
	return out
}

// A refusal for the lagging image stands for the slings behind it: the tick
// calls Sling once, the other three are not tried and the refusal is still the
// deferral it was (no failure label), gt-uisom.
func TestSpecTickStopsSlingingAfterAnImageDriftRefusal(t *testing.T) {
	t.Parallel()
	f := imageDriftTown()
	env, calls := driftEnv(t, f, 1)

	r := runSpecDispatchCycle(env)

	if *calls != 1 {
		t.Fatalf("%s: env.Sling called %d times, want 1", t.Name(), *calls)
	}
	if len(r.Dispatched) != 0 || len(f.slung) != 0 {
		t.Errorf("%s: dispatched %+v, slung %v; want nothing", t.Name(), r.Dispatched, f.slung)
	}
	lines := notTriedLines(r)
	if len(lines) != 3 {
		t.Fatalf("%s: skipped = %+v, want three 'not tried' entries", t.Name(), r.Skipped)
	}
	for _, l := range lines {
		if !strings.Contains(l, "the sandbox image lags the installed gt (a sling to sbx-flash was refused for it 0s ago)") {
			t.Errorf("%s: skip line = %q", t.Name(), l)
		}
	}
	if len(r.Deferred) != 4 {
		t.Errorf("%s: deferred = %+v, want the refusal and the three not-tried skips", t.Name(), r.Deferred)
	}
	if len(f.labels) != 0 || len(f.notes) != 0 {
		t.Errorf("%s: labels = %v notes = %v, want no bead write: a drift deferral is not a failure and a held sling writes nothing", t.Name(), f.labels, f.notes)
	}
}

// The daemon hands every planned sling to a `gt spec sling --job` process, so
// the hold has to cross processes: a job refused for the image leaves the
// marker, the next job does not try, and the next tick starts no job.
func TestSpecBackgroundSlingsHoldAfterAnImageDriftRefusal(t *testing.T) {
	t.Parallel()
	f := imageDriftTown()
	env, calls := driftEnv(t, f, 1)
	env.Background = true

	first := runSpecDispatchCycle(env)
	if len(first.Pending) != 4 || *calls != 0 {
		t.Fatalf("%s: first tick pending %d, Sling calls %d; want 4 jobs planned and none run", t.Name(), len(first.Pending), *calls)
	}
	// Two job processes, one after the other, each with its own env over the
	// same marker file.
	for i, job := range first.Pending[:2] {
		jr := runSpecSlingJob(env, job)
		if i == 0 && len(jr.Skipped) != 1 {
			t.Fatalf("%s: job 1 report = %+v, want the refusal as a skip", t.Name(), jr)
		}
		if i == 1 && (len(notTriedLines(jr)) != 1 || *calls != 1) {
			t.Errorf("%s: job 2 report = %+v, Sling calls %d; want it not tried", t.Name(), jr, *calls)
		}
	}

	second := runSpecDispatchCycle(env)
	if len(second.Pending) != 0 {
		t.Errorf("%s: the next tick planned %d jobs, want none while the refusal stands", t.Name(), len(second.Pending))
	}
	if n := len(notTriedLines(second)); n != 4 {
		t.Errorf("%s: the next tick skipped %d beads as not tried, want 4: %+v", t.Name(), n, second.Skipped)
	}
}

// Only session.ErrImageDrift leaves the marker: a cache volume the seat lacks
// or contention that outlasts its retries is deferred for one bead and the tick
// keeps slinging the rest.
func TestSpecTickKeepsSlingingAfterOtherDeferrals(t *testing.T) {
	t.Parallel()
	contention := errors.New("formula failed: bd mol bond: Error 1213 (40001): serialization failure")
	cases := []struct {
		name   string
		errFor func(n int) error
	}{
		{"the sandbox cache volume cannot be created", func(n int) error {
			if n == 0 {
				return fmt.Errorf("starting polecat session: %w", fmt.Errorf("creating sandbox: %w", session.ErrCacheVolume))
			}
			return nil
		}},
		{"contention outlasts every retry", func(n int) error {
			if n < specdispatch.RetryAttempts {
				return contention
			}
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := imageDriftTown()
			env, _ := driftEnv(t, f, 0)
			inner := env.Sling
			n := 0
			env.Sling = func(c specCandidate, resume string, seat specdispatch.SeatChoice) (string, error) {
				err := tc.errFor(n)
				n++
				if err != nil {
					return "", err
				}
				return inner(c, resume, seat)
			}
			r := runSpecDispatchCycle(env)
			if len(r.Dispatched) != 3 || len(r.Deferred) != 1 {
				t.Errorf("%s: dispatched %+v deferred %+v, want three slung and one deferred", t.Name(), r.Dispatched, r.Deferred)
			}
			if lines := notTriedLines(r); len(lines) != 0 {
				t.Errorf("%s: %v stopped the tick after a non-drift deferral", t.Name(), lines)
			}
		})
	}
}

// The hold is two minutes: after it one sling tries again, and a sling that
// reaches a session clears the marker so the rest of the tick goes on.
func TestSpecImageDriftHoldAgesOutAndASuccessClearsIt(t *testing.T) {
	t.Parallel()
	f := imageDriftTown()
	env, calls := driftEnv(t, f, 1)
	if r := runSpecDispatchCycle(env); len(notTriedLines(r)) != 3 {
		t.Fatalf("%s: first tick = %+v, want three not tried", t.Name(), r.Skipped)
	}

	env.Now = func() time.Time { return specTestNow.Add(imagedrift.HoldFor + time.Second) }
	r := runSpecDispatchCycle(env)

	if len(r.Dispatched) != 4 || len(notTriedLines(r)) != 0 {
		t.Errorf("%s: after the hold: dispatched %+v, not tried %v; want all four slung", t.Name(), r.Dispatched, notTriedLines(r))
	}
	if *calls != 5 {
		t.Errorf("%s: Sling calls = %d, want 5 (the refusal and then four)", t.Name(), *calls)
	}
	if held, _, _ := env.Drift.Held(env.Now(), "sbx-flash"); held {
		t.Errorf("%s: the marker still holds after a sling reached a session", t.Name())
	}
}

// An unreadable marker holds nothing: the sling is tried as before the marker
// existed. A marker that cannot be written or cleared is said in the line and
// does not fail the sling.
func TestSpecImageDriftMarkerFailuresDoNotStopASling(t *testing.T) {
	t.Parallel()
	t.Run("an unreadable marker", func(t *testing.T) {
		t.Parallel()
		f := imageDriftTown()
		env, calls := driftEnv(t, f, 0)
		dir := t.TempDir()
		path := filepath.Join(dir, imagedrift.FileName)
		if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
			t.Fatalf("%s: %v", t.Name(), err)
		}
		env.Drift = imagedrift.StoreIn(path)
		r := runSpecDispatchCycle(env)
		if *calls != 4 || len(r.Dispatched) != 4 {
			t.Errorf("%s: Sling calls %d dispatched %d, want 4 and 4", t.Name(), *calls, len(r.Dispatched))
		}
		// The first sling says the hold check failed; its success then clears
		// the broken file, so the slings after it read no marker at all.
		said := 0
		for _, e := range r.Dispatched {
			if strings.Contains(e.Line, "image-drift marker unreadable, not holding") {
				said++
			}
		}
		if said != 1 {
			t.Errorf("%s: %d lines say the hold check failed, want exactly the first sling's: %+v", t.Name(), said, r.Dispatched)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: the broken marker was not cleared by the sling that reached a session: %v", t.Name(), err)
		}
	})
	t.Run("a marker that cannot be written", func(t *testing.T) {
		t.Parallel()
		f := imageDriftTown()
		env, _ := driftEnv(t, f, 1)
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatalf("%s: %v", t.Name(), err)
		}
		env.Drift = imagedrift.StoreIn(filepath.Join(blocker, imagedrift.FileName))
		r := runSpecDispatchCycle(env)
		found := false
		for _, e := range r.Deferred {
			if strings.Contains(e.Line, "image-drift marker not written") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no deferral line says the marker was not written: %+v", t.Name(), r.Deferred)
		}
	})
	t.Run("a marker that cannot be cleared", func(t *testing.T) {
		t.Parallel()
		f := imageDriftTown()
		env, _ := driftEnv(t, f, 0)
		dir := filepath.Join(t.TempDir(), imagedrift.FileName)
		if err := os.MkdirAll(filepath.Join(dir, "inside"), 0o755); err != nil {
			t.Fatalf("%s: %v", t.Name(), err)
		}
		env.Drift = imagedrift.StoreIn(dir)
		r := runSpecDispatchCycle(env)
		if len(r.Dispatched) != 4 || !strings.Contains(r.Dispatched[0].Line, "image-drift marker not cleared") {
			t.Errorf("%s: dispatched %+v, want four slung with the clear failure said", t.Name(), r.Dispatched)
		}
	})
}
