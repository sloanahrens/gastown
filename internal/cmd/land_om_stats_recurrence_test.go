package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// TestLandOmStatsRecurrenceTalliesCounts: two minors on a held-out bead are
// held out and one on a filed bead is filed, while a queue line, a no-bead
// record, a minor of another class and a major count on neither side.
func TestLandOmStatsRecurrenceTalliesCounts(t *testing.T) {
	t.Parallel()
	const held, filed = "gt-rec-0004", "gt-rec-0000"
	class, in := omlessons.HoldOutClasses[0], omlessons.HoldOutStart.Add(time.Hour)
	if !omlessons.HeldOut(held, class, in) || omlessons.HeldOut(filed, class, in) {
		t.Fatalf("fixture beads %s and %s are not on the sides their names say", held, filed)
	}
	doc := land.ReviewFindingText{Severity: "minor", Title: "the documentation says the flag defaults to true"}
	scope := land.ReviewFindingText{Severity: "minor", Title: "the change reaches outside the scope"}
	major := land.ReviewFindingText{Severity: "major", Title: "the documentation says the flag defaults to true"}
	records := []land.ReviewRecord{
		{BeadID: held, At: in, FindingText: []land.ReviewFindingText{doc, doc, scope}},
		{BeadID: held, At: in, Queue: &land.FollowupQueue{}, FindingText: []land.ReviewFindingText{doc}},
		{At: in, FindingText: []land.ReviewFindingText{doc}},
		{BeadID: filed, At: in, FindingText: []land.ReviewFindingText{doc, major}},
	}
	row := recurrence(records, in)[0]
	if row.ID != class || row.HeldOut.Findings != 2 || row.Filed.Findings != 1 ||
		row.HeldOut.Followed != 0 || row.Filed.Followed != 0 {
		t.Errorf("row = %+v, want %q with 2 held out, 1 filed, none followed", row, class)
	}
}

// TestLandOmStatsRecurrenceClock: a finding recorded after now is not tallied,
// one at now is, and an earlier now counts fewer than a later one, because a
// record the report cannot yet have seen is no evidence (gt-c9sr9p).
func TestLandOmStatsRecurrenceClock(t *testing.T) {
	t.Parallel()
	doc := land.ReviewFindingText{Severity: "minor", Title: "the documentation says the flag defaults to true"}
	now := omlessons.HoldOutStart.Add(48 * time.Hour)
	records := []land.ReviewRecord{
		{BeadID: "gt-rec-0000", At: now.Add(-49 * time.Hour), FindingText: []land.ReviewFindingText{doc}}, // before the window
		{BeadID: "gt-rec-0000", At: now.Add(-time.Hour), FindingText: []land.ReviewFindingText{doc}},      // before now
		{BeadID: "gt-rec-0000", At: now, FindingText: []land.ReviewFindingText{doc}},                      // at now
		{BeadID: "gt-rec-0000", At: now.Add(time.Hour), FindingText: []land.ReviewFindingText{doc}},       // after now
	}
	count := func(at time.Time) int {
		row := recurrence(records, at)[0]
		return row.HeldOut.Findings + row.Filed.Findings
	}
	if got := count(now); got != 2 {
		t.Errorf("at now: %d tallied, want 2 (one before and one at now, none after)", got)
	}
	if got := count(now.Add(-2 * time.Hour)); got != 0 {
		t.Errorf("two hours earlier: %d tallied, want 0", got)
	}
	if earlier, later := count(now.Add(-time.Hour)), count(now.Add(time.Hour)); earlier >= later {
		t.Errorf("earlier now tallied %d, later now %d; want earlier < later", earlier, later)
	}
}

// TestLandOmStatsRecurrenceTalliesWindow: a finding counts from the window's
// first instant up to but not including its end.
func TestLandOmStatsRecurrenceTalliesWindow(t *testing.T) {
	t.Parallel()
	class, start := omlessons.HoldOutClasses[0], omlessons.HoldOutStart
	end := start.AddDate(0, 0, omlessons.HoldOutDays)
	doc := land.ReviewFindingText{Severity: "minor", Title: "the documentation says the flag defaults to true"}
	for _, c := range []struct {
		at   time.Time
		want int
	}{{start.Add(-time.Second), 0}, {start, 1}, {end.Add(-time.Second), 1}, {end, 0}} {
		row := recurrence([]land.ReviewRecord{{BeadID: "gt-rec-0000", At: c.at, FindingText: []land.ReviewFindingText{doc}}}, end)[0]
		if got := row.HeldOut.Findings + row.Filed.Findings; got != c.want || row.ID != class {
			t.Errorf("finding at %s: %d counted, want %d", c.at, got, c.want)
		}
	}
}
