package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
