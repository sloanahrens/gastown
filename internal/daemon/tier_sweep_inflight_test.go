package daemon

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plantMark puts a mark at path: a directory when dir, else the given bytes.
func plantMark(t *testing.T, path, body string, dir bool) {
	t.Helper()
	var err error
	if dir {
		err = os.MkdirAll(path, 0o755)
	} else if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		err = os.WriteFile(path, []byte(body), 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestTierSweepInFlightMarkRoundTrip: a written mark reads back its commit and
// start time from the tier-sweep directory, no mark reads as fs.ErrNotExist,
// and clearing removes it and clearing again is no error (gt-rfsbkw).
func TestTierSweepInFlightMarkRoundTrip(t *testing.T) {
	t.Parallel()
	town, started := t.TempDir(), time.Date(2026, 11, 27, 5, 13, 0, 0, time.UTC)
	if _, err := readTierSweepInFlight(town, "gastown"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read with no mark = %v, want fs.ErrNotExist", err)
	}
	if err := writeTierSweepInFlight(town, "gastown", tierSweepInFlightMark{SHA: "abc123", Started: started}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := tierSweepInFlightPath(town, "gastown"), filepath.Join(town, ".runtime", "tier-sweep", "gastown.inflight.json"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if mark, err := readTierSweepInFlight(town, "gastown"); err != nil || mark.SHA != "abc123" || !mark.Started.Equal(started) {
		t.Errorf("read = %+v, %v, want abc123 started %v", mark, err, started)
	}
	if err := clearTierSweepInFlight(town, "gastown"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := readTierSweepInFlight(town, "gastown"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read after clear = %v, want fs.ErrNotExist", err)
	}
	if err := clearTierSweepInFlight(town, "gastown"); err != nil {
		t.Errorf("clear with no mark = %v, want nil", err)
	}
}

// TestTierSweepInFlightMarkRefusesOneItCannotUse: a mark that is unreadable, or
// does not parse, or names no commit or no start time, is an error.
func TestTierSweepInFlightMarkRefusesOneItCannotUse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body string
		dir        bool
	}{
		{"unreadable", "", true},
		{"not JSON", "{not json", false},
		{"no commit", `{"started":"2026-11-27T05:13:00Z"}`, false},
		{"no start time", `{"sha":"abc123"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			plantMark(t, tierSweepInFlightPath(town, "gastown"), tc.body, tc.dir)
			if mark, err := readTierSweepInFlight(town, "gastown"); err == nil {
				t.Errorf("read = %+v, want an error", mark)
			}
		})
	}
}

// TestTierSweepInFlightMarkReportsFailures: a mark that cannot be written or
// removed returns the error rather than reading as written or cleared.
func TestTierSweepInFlightMarkReportsFailures(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := tierSweepInFlightPath(town, "gastown")
	plantMark(t, path, "", true)
	mark := tierSweepInFlightMark{SHA: "abc123", Started: time.Date(2026, 11, 27, 5, 13, 0, 0, time.UTC)}
	if err := writeTierSweepInFlight(town, "gastown", mark); err == nil {
		t.Error("write over a directory = nil, want an error")
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := clearTierSweepInFlight(town, "gastown"); err == nil {
		t.Error("clear of a non-empty directory = nil, want an error")
	}
}

// TestTierSweepResumeSHA: a mark younger than the run budget names the commit
// to sweep and one line names the tip it replaces; a mark at or past the
// budget, and no mark, give the tip silently; an unreadable mark gives the tip
// and is logged (gt-ejsew3).
func TestTierSweepResumeSHA(t *testing.T) {
	t.Parallel()
	const (
		markSHA = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
		tipSHA  = "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
	)
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		started time.Time
		markSHA string
		unread  bool
		want    string
		logged  []string
	}{
		{"young mark resumes", now.Add(-time.Minute), markSHA, false, markSHA, []string{"resuming", "aaaa1111", "bbbb2222"}},
		{"mark at the budget", now.Add(-tierSweepRunBudget), markSHA, false, tipSHA, nil},
		{"mark past the budget", now.Add(-tierSweepRunBudget - time.Second), markSHA, false, tipSHA, nil},
		{"no mark", time.Time{}, "", false, tipSHA, nil},
		{"young mark already the tip", now.Add(-time.Minute), tipSHA, false, tipSHA, nil},
		{"unreadable mark", time.Time{}, "", true, tipSHA, []string{"reading the in-flight mark"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
			switch {
			case tc.unread:
				plantMark(t, tierSweepInFlightPath(d.config.TownRoot, "gastown"), "{not json", false)
			case !tc.started.IsZero():
				if err := writeTierSweepInFlight(d.config.TownRoot, "gastown", tierSweepInFlightMark{SHA: tc.markSHA, Started: tc.started}); err != nil {
					t.Fatalf("write mark: %v", err)
				}
			}
			if got := d.tierSweepResumeSHA("gastown", tipSHA, now); got != tc.want {
				t.Errorf("resume sha = %q, want %q", got, tc.want)
			}
			for _, want := range tc.logged {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log missing %q; got:\n%s", want, logs.String())
				}
			}
			if len(tc.logged) == 0 && logs.Len() != 0 {
				t.Errorf("logged %q, want silence", logs.String())
			}
		})
	}
}

// TestTierSweepMarksTheCommitUnderSweep: while a stage runs the mark names the
// commit under sweep and its start; a recorded cycle, one whose record write
// failed and one no stage ran in leave none; a cycle whose context ended keeps
// it; a mark that cannot be written or cleared is logged and changes nothing
// else (gt-nz89cq).
func TestTierSweepMarksTheCommitUnderSweep(t *testing.T) {
	t.Parallel()
	const sha = "abc123abc123abc123abc123abc123abc123abc1"
	now := atHour(time.Now(), 15)
	// cycleDaemon is a daemon whose cycle checks out and reaches both stages at sha.
	cycleDaemon := func(t *testing.T) (*Daemon, *tierSweepRunRecorder, *bytes.Buffer) {
		d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
		d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
		d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
		return d, rec, logs
	}
	// wantMark asserts the mark names sha started at now, or is gone.
	wantMark := func(t *testing.T, d *Daemon, want bool) {
		mark, err := readTierSweepInFlight(d.config.TownRoot, "gastown")
		if want && (err != nil || mark.SHA != sha || !mark.Started.Equal(now)) {
			t.Errorf("mark = %+v, %v; want %s started %v", mark, err, sha, now)
		} else if !want && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("mark = %+v, %v; want fs.ErrNotExist", mark, err)
		}
	}

	t.Run("while a stage runs, gone once recorded", func(t *testing.T) {
		t.Parallel()
		d, rec, _ := cycleDaemon(t)
		rec.green()
		green := rec.result
		rec.result = func(st tierSweepStage) tierSweepStageResult { wantMark(t, d, true); return green(st) }
		if !d.runTierSweep() {
			t.Fatal("runTierSweep deferred; want a verdict")
		}
		wantMark(t, d, false)
	})

	t.Run("gone after a failed record write", func(t *testing.T) {
		t.Parallel()
		d, rec, _ := cycleDaemon(t)
		rec.green()
		if err := os.MkdirAll(tierSweepStatePath(d.config.TownRoot, "gastown"), 0o755); err != nil {
			t.Fatal(err)
		}
		if d.runTierSweep() {
			t.Error("runTierSweep = true, want false with the record unwritable")
		}
		wantMark(t, d, false)
	})

	t.Run("gone after a cycle no stage ran in", func(t *testing.T) {
		t.Parallel()
		d, rec, _ := cycleDaemon(t)
		rec.result = func(tierSweepStage) tierSweepStageResult { return tierSweepStageResult{} }
		if d.runTierSweep() {
			t.Error("runTierSweep = true, want false with no stage run")
		}
		wantMark(t, d, false)
	})

	t.Run("kept when the cycle's context ended", func(t *testing.T) {
		t.Parallel()
		d, rec, _ := cycleDaemon(t)
		rec.green()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cycle := d.startDogCycle("tier_sweep")
		defer cycle.close()
		if !d.runTierSweepRig(ctx, cycle, "gastown", now) {
			t.Error("runTierSweepRig deferred; want the cycle's verdict")
		}
		wantMark(t, d, true)
	})

	for _, tc := range []struct {
		name, logged string
		breakMark    func(*Daemon, *tierSweepRunRecorder, *testing.T)
	}{
		{"written", "writing the mark of the commit under sweep", func(d *Daemon, _ *tierSweepRunRecorder, t *testing.T) {
			plantMark(t, tierSweepInFlightPath(d.config.TownRoot, "gastown"), "", true)
		}},
		{"cleared", "clearing the mark of the commit under sweep", func(d *Daemon, rec *tierSweepRunRecorder, t *testing.T) {
			green, swapped := rec.result, false
			rec.result = func(st tierSweepStage) tierSweepStageResult {
				if path := tierSweepInFlightPath(d.config.TownRoot, "gastown"); !swapped {
					swapped = true
					if err := os.Remove(path); err != nil {
						t.Fatalf("removing the written mark: %v", err)
					}
					plantMark(t, path, "", true)
					if err := os.WriteFile(filepath.Join(path, "keep"), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return green(st)
			}
		}},
	} {
		t.Run("a mark it cannot be "+tc.name+" is logged", func(t *testing.T) {
			t.Parallel()
			d, rec, logs := cycleDaemon(t)
			rec.green()
			tc.breakMark(d, rec, t)
			if !d.runTierSweep() {
				t.Error("runTierSweep deferred; a mark failure must not change the verdict")
			}
			if !strings.Contains(logs.String(), tc.logged) {
				t.Errorf("log missing %q; got:\n%s", tc.logged, logs.String())
			}
		})
	}
}

// TestTierSweepResumesAnInterruptedSweep: a cycle started with a young mark and
// a newer origin/main checks out, tests and records the marked commit instead
// of the tip; with no mark it sweeps the tip (gt-vbdeh9).
func TestTierSweepResumesAnInterruptedSweep(t *testing.T) {
	t.Parallel()
	const marked, tip = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111", "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		mark bool
		want string
	}{
		{"young mark resumes the interrupted commit", true, marked},
		{"no mark sweeps the tip", false, tip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return tip, nil }
			var checkedOut []string
			d.tierSweepSeams.worktree = func(_ context.Context, _, _, sha string) (func(), error) {
				checkedOut = append(checkedOut, sha)
				return func() {}, nil
			}
			if tc.mark {
				mark := tierSweepInFlightMark{SHA: marked, Started: now.Add(-time.Minute)}
				if err := writeTierSweepInFlight(d.config.TownRoot, "gastown", mark); err != nil {
					t.Fatalf("write mark: %v", err)
				}
			}
			rec.green()
			cycle := d.startDogCycle("tier_sweep")
			defer cycle.close()
			if !d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
				t.Fatal("the cycle deferred; both stages report a verdict")
			}
			if len(checkedOut) != 1 || checkedOut[0] != tc.want {
				t.Errorf("checked out %q, want one checkout of %s", checkedOut, tc.want)
			}
			if st := tierSweepTestState(t, d, "gastown"); st.LastSHA != tc.want || st.LastGreenSHA != tc.want {
				t.Errorf("record = last %s green %s, want both %s", st.LastSHA, st.LastGreenSHA, tc.want)
			}
		})
	}
}
