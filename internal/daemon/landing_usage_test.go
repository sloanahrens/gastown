package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// omUsageOnDisk is the record file the real writer keeps for a bead's om run:
// the usage record plus the ids of the sessions it counted.
type omUsageOnDisk struct {
	dashboard.UsageRecord
	Sessions []string `json:"sessions"`
}

func readOMUsage(t *testing.T, town, bead string) omUsageOnDisk {
	t.Helper()
	path := filepath.Join(constants.TownRuntimePath(town), "usage", "beads", bead+".om.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec omUsageOnDisk
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// fixedList is the transcript lister a test gives the sink: whatever the review
// directory, these are the files its project directories hold.
func fixedList(paths ...string) func(string, string) ([]string, error) {
	return func(string, string) ([]string, error) { return paths, nil }
}

// TestLandingUsageSumsTranscriptsOfTheRun: a review's record sums the transcripts
// its run wrote and leaves out an earlier run's, carries the bead and rig with
// kind om, and a second review adds its own session to the same record.
func TestLandingUsageSumsTranscriptsOfTheRun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	proj := t.TempDir()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	old := filepath.Join(proj, "old.jsonl")
	writeUsageTranscript(t, old, start.Add(-time.Hour), seatCallLine("old", 900, 0, 900))
	first := filepath.Join(proj, "first.jsonl")
	writeUsageTranscript(t, first, start.Add(30*time.Minute), seatCallLine("first", 100, 10, 20))
	second := filepath.Join(proj, "second.jsonl")
	writeUsageTranscript(t, second, start.Add(time.Hour), seatCallLine("second", 200, 0, 30))
	sink := &omUsageSink{townRoot: town, list: fixedList(old, first, second), logf: func(string, ...any) {}}

	sink.record(context.Background(), land.ReviewUsage{BeadID: "gt-abc", Rig: "gastown", Dir: proj,
		Start: start, End: start.Add(2 * time.Hour)})

	rec := readOMUsage(t, town, "gt-abc")
	if rec.Kind != dashboard.UsageKindOM || rec.Class != dashboard.UsageClassOMReview {
		t.Errorf("record kind %q class %q, want %q %q", rec.Kind, rec.Class, dashboard.UsageKindOM, dashboard.UsageClassOMReview)
	}
	if rec.Bead != "gt-abc" || rec.Rig != "gastown" {
		t.Errorf("record bead %q rig %q, want gt-abc gastown", rec.Bead, rec.Rig)
	}
	if got := strings.Join(rec.Sessions, ","); got != "first,second" {
		t.Errorf("counted sessions = %q, want the two written since the start (first,second)", got)
	}
	if rec.Totals.Calls != 2 || rec.Totals.Output != 50 {
		t.Errorf("totals calls %d output %d, want 2 and 50 from the two transcripts", rec.Totals.Calls, rec.Totals.Output)
	}

	// A second review of the same bead starts after the first one's transcripts
	// were last written, so it counts its own session and not theirs.
	secondStart := start.Add(90 * time.Minute)
	third := filepath.Join(proj, "third.jsonl")
	writeUsageTranscript(t, third, secondStart.Add(30*time.Minute), seatCallLine("third", 300, 0, 7))
	sink.list = fixedList(old, first, second, third)
	sink.record(context.Background(), land.ReviewUsage{BeadID: "gt-abc", Rig: "gastown", Dir: proj,
		Start: secondStart, End: secondStart.Add(2 * time.Hour)})

	rec = readOMUsage(t, town, "gt-abc")
	if got := strings.Join(rec.Sessions, ","); got != "first,second,third" {
		t.Errorf("after the second review counted sessions = %q, want first,second,third", got)
	}
	if rec.Totals.Calls != 3 || rec.Totals.Output != 57 {
		t.Errorf("after the second review calls %d output %d, want 3 and 57", rec.Totals.Calls, rec.Totals.Output)
	}
}

// TestLandingUsageNoTranscriptWritesNothing: a review that wrote no transcript
// writes no record and says so once for its bead. A write or read that fails is
// logged and dropped, so the landing never sees it.
func TestLandingUsageNoTranscriptWritesNothing(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	window := land.ReviewUsage{BeadID: "gt-abc", Rig: "gastown", Dir: t.TempDir(), Start: start, End: start.Add(time.Hour)}

	t.Run("no transcript writes nothing and logs once across two reviews", func(t *testing.T) {
		town := t.TempDir()
		var logged []string
		writes := 0
		sink := &omUsageSink{townRoot: town, list: fixedList(), logf: func(f string, a ...any) {
			logged = append(logged, fmt.Sprintf(f, a...))
		}, write: func(string, dashboard.UsageRecord, []string) error {
			writes++
			return nil
		}}
		sink.record(context.Background(), window)
		sink.record(context.Background(), window)
		if writes != 0 {
			t.Errorf("writes = %d, want none for a review with no transcript", writes)
		}
		if n := loggedLinesWith(logged, "wrote no transcript"); n != 1 {
			t.Errorf("no-transcript note logged %d times across two reviews, want once: %q", n, logged)
		}
	})

	t.Run("a failing write is logged and does not fail the review", func(t *testing.T) {
		proj := t.TempDir()
		transcript := filepath.Join(proj, "run.jsonl")
		writeUsageTranscript(t, transcript, start.Add(time.Minute), seatCallLine("run", 10, 0, 1))
		var logged []string
		sink := &omUsageSink{townRoot: t.TempDir(), list: fixedList(transcript), logf: func(f string, a ...any) {
			logged = append(logged, fmt.Sprintf(f, a...))
		}, write: func(string, dashboard.UsageRecord, []string) error {
			return errors.New("disk full")
		}}
		sink.record(context.Background(), window)
		if n := loggedLinesWith(logged, "disk full"); n != 1 {
			t.Errorf("the write failure logged %d times, want once: %q", n, logged)
		}
	})

	t.Run("a listing error writes nothing and is logged", func(t *testing.T) {
		var logged []string
		writes := 0
		sink := &omUsageSink{townRoot: t.TempDir(), list: func(string, string) ([]string, error) {
			return nil, errors.New("unreadable project directory")
		}, logf: func(f string, a ...any) {
			logged = append(logged, fmt.Sprintf(f, a...))
		}, write: func(string, dashboard.UsageRecord, []string) error {
			writes++
			return nil
		}}
		sink.record(context.Background(), window)
		if writes != 0 || loggedLinesWith(logged, "unreadable project directory") != 1 {
			t.Errorf("writes = %d, logged %q; want no write and the listing error logged once", writes, logged)
		}
	})

	t.Run("an unreadable workspace map writes nothing and is logged", func(t *testing.T) {
		town := t.TempDir()
		mapPath := filepath.Join(town, workspaceid.File)
		if err := os.MkdirAll(filepath.Dir(mapPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mapPath, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		proj := t.TempDir()
		transcript := filepath.Join(proj, "run.jsonl")
		writeUsageTranscript(t, transcript, start.Add(time.Minute), seatCallLine("run", 10, 0, 1))
		var logged []string
		writes := 0
		sink := &omUsageSink{townRoot: town, list: fixedList(transcript), logf: func(f string, a ...any) {
			logged = append(logged, fmt.Sprintf(f, a...))
		}, write: func(string, dashboard.UsageRecord, []string) error {
			writes++
			return nil
		}}
		sink.record(context.Background(), window)
		if writes != 0 || loggedLinesWith(logged, "not written") != 1 {
			t.Errorf("writes = %d, logged %q; want no write and the refusal logged once", writes, logged)
		}
	})
}

// TestLandingUsageUnparseableTranscriptIsLoggedAndMarked: a transcript with a
// line that does not parse is still counted, its record is marked incomplete,
// and the read error is logged, not returned to the landing.
func TestLandingUsageUnparseableTranscriptIsLoggedAndMarked(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	proj := t.TempDir()
	transcript := filepath.Join(proj, "run.jsonl")
	writeUsageTranscript(t, transcript, start.Add(time.Minute), seatCallLine("run", 10, 0, 1)+"not json\n")
	var logged []string
	var got dashboard.UsageRecord
	writes := 0
	sink := &omUsageSink{townRoot: t.TempDir(), list: fixedList(transcript), logf: func(f string, a ...any) {
		logged = append(logged, fmt.Sprintf(f, a...))
	}, write: func(_ string, rec dashboard.UsageRecord, _ []string) error {
		writes++
		got = rec
		return nil
	}}
	sink.record(context.Background(), land.ReviewUsage{BeadID: "gt-abc", Rig: "gastown", Dir: proj,
		Start: start, End: start.Add(time.Hour)})
	if writes != 1 {
		t.Fatalf("writes = %d, want the record written with its readable calls", writes)
	}
	if got.Complete {
		t.Errorf("record is complete, want incomplete: a transcript line did not parse")
	}
	if got.Totals.Calls != 1 {
		t.Errorf("calls = %d, want the one readable call counted", got.Totals.Calls)
	}
	if n := loggedLinesWith(logged, "do not parse"); n != 1 {
		t.Errorf("parse error logged %d times, want once: %q", n, logged)
	}
}

// TestLandingUsageHonoursEndBound: of four transcripts, the record sums the two
// last written inside the review's window. One written before the start is an
// earlier run's and one written after the end is the next round's; both are
// left out. A transcript written exactly at the end counts. A second review
// adds its own session to the same record, and the late transcript stays out.
func TestLandingUsageHonoursEndBound(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	proj := t.TempDir()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	before := filepath.Join(proj, "before.jsonl")
	writeUsageTranscript(t, before, start.Add(-time.Minute), seatCallLine("before", 900, 0, 900))
	inA := filepath.Join(proj, "in-a.jsonl")
	writeUsageTranscript(t, inA, start.Add(10*time.Minute), seatCallLine("in-a", 100, 0, 10))
	inB := filepath.Join(proj, "in-b.jsonl")
	writeUsageTranscript(t, inB, end, seatCallLine("in-b", 200, 0, 20))
	late := filepath.Join(proj, "late.jsonl")
	writeUsageTranscript(t, late, end.Add(time.Second), seatCallLine("late", 5000, 0, 5000))
	sink := &omUsageSink{townRoot: town, list: fixedList(before, inA, inB, late), logf: func(string, ...any) {}}

	sink.record(context.Background(), land.ReviewUsage{BeadID: "gt-abc", Rig: "gastown", Dir: proj,
		Start: start, End: end})

	rec := readOMUsage(t, town, "gt-abc")
	if got := strings.Join(rec.Sessions, ","); got != "in-a,in-b" {
		t.Errorf("counted sessions = %q, want only the two inside the window (in-a,in-b)", got)
	}
	if rec.Totals.Calls != 2 || rec.Totals.Input != 300 || rec.Totals.Output != 30 {
		t.Errorf("totals calls %d input %d output %d, want 2, 300 and 30", rec.Totals.Calls, rec.Totals.Input, rec.Totals.Output)
	}
	if !rec.Complete {
		t.Errorf("record is incomplete, want complete: both transcripts were read whole")
	}

	// The second review opens after the late transcript was written, so that
	// transcript is still the next round's and is excluded from it too.
	secondStart := end.Add(time.Minute)
	third := filepath.Join(proj, "third.jsonl")
	writeUsageTranscript(t, third, secondStart.Add(30*time.Minute), seatCallLine("third", 300, 0, 7))
	sink.list = fixedList(before, inA, inB, late, third)
	sink.record(context.Background(), land.ReviewUsage{BeadID: "gt-abc", Rig: "gastown", Dir: proj,
		Start: secondStart, End: secondStart.Add(2 * time.Hour)})

	rec = readOMUsage(t, town, "gt-abc")
	if got := strings.Join(rec.Sessions, ","); got != "in-a,in-b,third" {
		t.Errorf("after the second review counted sessions = %q, want in-a,in-b,third", got)
	}
	if rec.Totals.Calls != 3 || rec.Totals.Output != 37 {
		t.Errorf("after the second review calls %d output %d, want 3 and 37", rec.Totals.Calls, rec.Totals.Output)
	}
}

// TestLandingUsageReadErrorIsSurfaced: a read error is logged with its text and
// its path, never as a review that wrote no transcript. An unreadable directory
// writes nothing, since no session is counted. An unreadable transcript is
// counted, and its record is marked incomplete.
func TestLandingUsageReadErrorIsSurfaced(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	window := land.ReviewUsage{BeadID: "gt-abc", Rig: "gastown", Dir: t.TempDir(), Start: start, End: start.Add(time.Hour)}

	t.Run("unreadable directory is logged, not read as no transcript", func(t *testing.T) {
		t.Parallel()
		var logged []string
		writes := 0
		sink := &omUsageSink{townRoot: t.TempDir(), list: func(string, string) ([]string, error) {
			return nil, errors.New("reading Claude project directory /x/projects/h: permission denied")
		}, logf: func(f string, a ...any) {
			logged = append(logged, fmt.Sprintf(f, a...))
		}, write: func(string, dashboard.UsageRecord, []string) error {
			writes++
			return nil
		}}
		sink.record(context.Background(), window)
		if writes != 0 {
			t.Errorf("writes = %d, want none: no session of an unreadable directory is counted", writes)
		}
		if n := loggedLinesWith(logged, "/x/projects/h: permission denied"); n != 1 {
			t.Errorf("listing error logged %d times, want once with its path: %q", n, logged)
		}
		if n := loggedLinesWith(logged, "wrote no transcript"); n != 0 {
			t.Errorf("unreadable directory logged as no transcript: %q", logged)
		}
	})

	t.Run("unreadable transcript is logged with its path and marks the record incomplete", func(t *testing.T) {
		t.Parallel()
		proj := t.TempDir()
		good := filepath.Join(proj, "good.jsonl")
		writeUsageTranscript(t, good, start.Add(time.Minute), seatCallLine("good", 10, 0, 1))
		gone := filepath.Join(proj, "gone.jsonl")
		var logged []string
		var got dashboard.UsageRecord
		writes := 0
		sink := &omUsageSink{townRoot: t.TempDir(), list: fixedList(good, gone), logf: func(f string, a ...any) {
			logged = append(logged, fmt.Sprintf(f, a...))
		}, write: func(_ string, rec dashboard.UsageRecord, _ []string) error {
			writes++
			got = rec
			return nil
		}}
		sink.record(context.Background(), window)
		if writes != 1 {
			t.Fatalf("writes = %d, want the record of the readable transcript", writes)
		}
		if got.Complete {
			t.Errorf("record is complete, want incomplete: a transcript could not be read")
		}
		if n := loggedLinesWith(logged, gone); n != 1 {
			t.Errorf("unreadable transcript logged %d times with its path, want once: %q", n, logged)
		}
		if n := loggedLinesWith(logged, "wrote no transcript"); n != 0 {
			t.Errorf("unreadable transcript logged as no transcript: %q", logged)
		}
	})

	t.Run("only unreadable transcripts mark the record incomplete", func(t *testing.T) {
		t.Parallel()
		var got dashboard.UsageRecord
		writes := 0
		sink := &omUsageSink{townRoot: t.TempDir(), list: fixedList(filepath.Join(t.TempDir(), "gone.jsonl")),
			logf: func(string, ...any) {}, write: func(_ string, rec dashboard.UsageRecord, _ []string) error {
				writes++
				got = rec
				return nil
			}}
		sink.record(context.Background(), window)
		if writes != 1 || got.Complete {
			t.Errorf("writes = %d complete = %v, want one incomplete record for the transcript that could not be read", writes, got.Complete)
		}
	})

	t.Run("a transcript with no session id writes nothing and says so", func(t *testing.T) {
		t.Parallel()
		var logged []string
		writes := 0
		sink := &omUsageSink{townRoot: t.TempDir(), list: fixedList(filepath.Join(t.TempDir(), ".jsonl")),
			logf: func(f string, a ...any) {
				logged = append(logged, fmt.Sprintf(f, a...))
			}, write: func(string, dashboard.UsageRecord, []string) error {
				writes++
				return nil
			}}
		sink.record(context.Background(), window)
		if writes != 0 {
			t.Errorf("writes = %d, want none: no session could be read", writes)
		}
		if n := loggedLinesWith(logged, "none of its review transcripts could be read"); n != 1 {
			t.Errorf("no-readable-transcript note logged %d times, want once: %q", n, logged)
		}
		if n := loggedLinesWith(logged, "wrote no transcript"); n != 0 {
			t.Errorf("logged as no transcript: %q", logged)
		}
	})
}

// TestLandingUsageRealLayout: with the production lister and no fake, a review
// directory's transcripts are found in the Claude project directory its run
// wrote them to, and the record is the sum of the ones inside the window.
func TestLandingUsageRealLayout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	reviewDir := filepath.Join(town, "gastown", "review", "gt-abc")
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projDir, err := agentlog.ClaudeProjectDirIn(filepath.Join(town, ".claude-town"), reviewDir)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	writeUsageTranscript(t, filepath.Join(projDir, "run-a.jsonl"), start.Add(5*time.Minute), seatCallLine("run-a", 100, 0, 10))
	writeUsageTranscript(t, filepath.Join(projDir, "run-b.jsonl"), start.Add(20*time.Minute), seatCallLine("run-b", 200, 50, 20))
	writeUsageTranscript(t, filepath.Join(projDir, "earlier.jsonl"), start.Add(-time.Hour), seatCallLine("earlier", 9000, 0, 9000))
	writeUsageTranscript(t, filepath.Join(projDir, "later.jsonl"), end.Add(time.Hour), seatCallLine("later", 9000, 0, 9000))
	var logged []string
	sink := &omUsageSink{townRoot: town, logf: func(f string, a ...any) {
		logged = append(logged, fmt.Sprintf(f, a...))
	}}

	sink.record(context.Background(), land.ReviewUsage{BeadID: "gt-abc", Rig: "gastown", Dir: reviewDir,
		Start: start, End: end})

	if len(logged) != 0 {
		t.Errorf("logged %q, want nothing for a clean run", logged)
	}
	rec := readOMUsage(t, town, "gt-abc")
	if got := strings.Join(rec.Sessions, ","); got != "run-a,run-b" {
		t.Errorf("counted sessions = %q, want run-a,run-b", got)
	}
	if rec.Totals.Calls != 2 || rec.Totals.Input != 300 || rec.Totals.CacheRead != 50 || rec.Totals.Output != 30 {
		t.Errorf("totals calls %d input %d cache read %d output %d, want 2, 300, 50 and 30",
			rec.Totals.Calls, rec.Totals.Input, rec.Totals.CacheRead, rec.Totals.Output)
	}
	if !rec.Complete {
		t.Errorf("record is incomplete, want complete")
	}
}

// TestLandingUsageRefusesUnsafeBeadID: a bead id that could name a file outside
// the usage directory, or is not a plain id, is refused with a logged error
// before any transcript is listed, and no file is written. A plain id writes
// its record.
func TestLandingUsageRefusesUnsafeBeadID(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{"", "..", "../escape", "a/b", `a\b`, "gt..x", "gt abc", "gt:abc", "gt\x00"} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			listed := 0
			var logged []string
			sink := &omUsageSink{townRoot: town, list: func(string, string) ([]string, error) {
				listed++
				return []string{filepath.Join(t.TempDir(), "run.jsonl")}, nil
			}, logf: func(f string, a ...any) {
				logged = append(logged, fmt.Sprintf(f, a...))
			}}

			sink.record(context.Background(), land.ReviewUsage{BeadID: id, Rig: "gastown", Dir: t.TempDir(),
				Start: start, End: start.Add(time.Hour)})

			if listed != 0 {
				t.Errorf("transcripts listed %d times for an unsafe id, want none", listed)
			}
			if n := loggedLinesWith(logged, "not a plain id"); n != 1 {
				t.Errorf("refusal logged %d times, want once: %q", n, logged)
			}
			if _, err := os.Stat(filepath.Join(constants.TownRuntimePath(town), "usage")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("usage directory exists (stat err %v), want no file written for an unsafe id", err)
			}
		})
	}
	t.Run("a plain id writes its record", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		proj := t.TempDir()
		writeUsageTranscript(t, filepath.Join(proj, "run.jsonl"), start.Add(time.Minute), seatCallLine("run", 10, 0, 1))
		sink := &omUsageSink{townRoot: town, list: fixedList(filepath.Join(proj, "run.jsonl")), logf: func(string, ...any) {}}

		sink.record(context.Background(), land.ReviewUsage{BeadID: "gt-6qc3nw.13", Rig: "gastown", Dir: proj,
			Start: start, End: start.Add(time.Hour)})

		if rec := readOMUsage(t, town, "gt-6qc3nw.13"); rec.Totals.Calls != 1 {
			t.Errorf("record calls = %d, want 1", rec.Totals.Calls)
		}
	})
}

// loggedLinesWith counts the lines that hold sub.
func loggedLinesWith(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}
