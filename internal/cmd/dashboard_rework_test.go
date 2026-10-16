package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
)

var reworkReaderNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// The dashboard cannot import the landing package, so the loop cap it counts
// against is its own constant; this holds it to the worker's.
func TestReworkLoopCapIsTheLandingWorkersAttemptCap(t *testing.T) {
	t.Parallel()
	if dashboard.ReworkLoopCap != land.MaxReworkAttempts {
		t.Fatalf("dashboard.ReworkLoopCap = %d, land.MaxReworkAttempts = %d", dashboard.ReworkLoopCap, land.MaxReworkAttempts)
	}
}

// What the landing worker writes is what the table counts: lines appended
// through the worker's own writer, read back and folded, with the lines older
// than both windows left out.
func TestReworkReaderFoldsWhatTheWorkerWrites(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	log := land.TownReviewsFile(root)
	at := func(h time.Duration) time.Time { return reworkReaderNow.Add(-h * time.Hour) }
	for _, rec := range []land.ReviewRecord{
		{BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.5, Verdict: "request_changes", Findings: 2, Bar: 0.8, At: at(30)},
		{BeadID: "gt-a", Rig: "gastown", Attempt: 2, Score: 0.9, Verdict: "approve", Bar: 0.8, At: at(20)},
		{BeadID: "gt-old", Rig: "gastown", Attempt: 1, Score: 0.9, Verdict: "approve", Bar: 0.8, At: at(24 * 30)},
	} {
		if err := log.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	got := newDashReworkReader(root).read(reworkReaderNow)
	if got.Unavailable || len(got.Windows) != 2 {
		t.Fatalf("read = %+v, want two windows", got)
	}
	bars := got.Windows[0].Bars
	if len(bars) != 1 || bars[0].Bar != 0.8 || bars[0].Reviews != 2 || bars[0].Rejected != 1 || bars[0].Beads != 1 ||
		bars[0].FirstTry != 0 || bars[0].Landed != 1 || bars[0].ReviewsPerLanded != 2 {
		t.Errorf("last 7 days = %+v, want gt-a's rejection and approval under 0.8", bars)
	}
	if len(got.Windows[1].Bars) != 0 {
		t.Errorf("the week before = %+v, want none: gt-old is a month old", got.Windows[1].Bars)
	}
}

// A log the landing worker has not created yet is a week with no reviews; one
// that cannot be read is an unavailable table, never an empty week.
func TestReworkReaderNamesAnUnreadableLog(t *testing.T) {
	t.Parallel()

	missing := newDashReworkReader(t.TempDir()).read(reworkReaderNow)
	if missing.Unavailable || len(missing.Windows) != 2 || len(missing.Windows[0].Bars) != 0 {
		t.Errorf("no log read = %+v, want two empty windows", missing)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".runtime", "landings", land.ReviewsFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := newDashReworkReader(root).read(reworkReaderNow); !got.Unavailable {
		t.Errorf("a log that is a directory read = %+v, want unavailable", got)
	}
}

// A bounced approval the worker logs with Rejected reaches the table as a
// rejection, not a first-try landing (gt-vu18r).
func TestReworkReaderCountsAFindingsBounce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := land.TownReviewsFile(root).Append(land.ReviewRecord{BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.93,
		Verdict: "approve", Rejected: true, Findings: 1, Bar: 0.8, At: reworkReaderNow.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	bars := newDashReworkReader(root).read(reworkReaderNow).Windows[0].Bars
	if len(bars) != 1 || bars[0].Rejected != 1 || bars[0].FirstTry != 0 || bars[0].Landed != 0 {
		t.Errorf("last 7 days = %+v, want one rejection and no first try", bars)
	}
}
