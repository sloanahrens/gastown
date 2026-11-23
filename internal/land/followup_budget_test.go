package land

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestFollowupBudgetDecision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		filed, landed int
		percent       int
		want          bool
	}{
		{"24 of 100 is under 25 percent", 24, 100, 25, true},
		{"25 of 100 is over 25 percent", 25, 100, 25, false},
		{"zero percent is the 25 default", 24, 100, 0, true},
		{"the first five beads are allowed with no landings", 4, 0, 25, true},
		{"the sixth bead with no landings is refused", 5, 0, 25, false},
		{"a negative percent is no budget", 1000, 0, -5, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BudgetAllows(c.filed, c.landed, c.percent); got != c.want {
				t.Errorf("BudgetAllows(%d, %d, %d) = %v, want %v", c.filed, c.landed, c.percent, got, c.want)
			}
		})
	}
}

func TestFollowupBudgetFiledSince(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	undated := ExistingFollowup{ID: "gt-undated"}
	cases := []struct {
		name     string
		existing []ExistingFollowup
		since    time.Time
		want     int
	}{
		{"at or after the cutoff counts and an undated bead does not", []ExistingFollowup{
			{ID: "gt-before", CreatedAt: cutoff.Add(-time.Hour)},
			{ID: "gt-at", CreatedAt: cutoff},
			{ID: "gt-after", CreatedAt: cutoff.Add(time.Minute)},
			undated,
		}, cutoff, 2},
		{"an undated bead is not counted from the zero cutoff", []ExistingFollowup{{ID: "gt-dated", CreatedAt: cutoff}, undated}, time.Time{}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FiledSince(c.existing, c.since); got != c.want {
				t.Errorf("FiledSince(%d bead(s), %v) = %d, want %d", len(c.existing), c.since, got, c.want)
			}
		})
	}
}

func TestFollowupBudgetLandedSince(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	f := &LandingsFile{Path: privateLandingsPath(t)}
	for _, rec := range []LandingRecord{
		{BeadID: "gt-before", LandedAt: cutoff.Add(-time.Hour)},
		{BeadID: "gt-at", LandedAt: cutoff},
		{BeadID: "gt-after", LandedAt: cutoff.Add(time.Hour)},
	} {
		if err := f.Append(rec); err != nil {
			t.Fatalf("Append(%s): %v", rec.BeadID, err)
		}
	}
	if got, err := LandedSince(f, cutoff); err != nil || got != 2 {
		t.Errorf("LandedSince = %d, %v; want 2 records at or after the cutoff, nil", got, err)
	}
	_, err := LandedSince(nil, cutoff)
	if err == nil || !strings.Contains(err.Error(), "landings file is nil") {
		t.Errorf("LandedSince(nil) error = %v, want one naming the nil landings file", err)
	}
	if _, err := LandedSince(&LandingsFile{Path: t.TempDir()}, cutoff); err == nil {
		t.Error("LandedSince on a directory = nil, want the read error")
	}
}

// TestFollowupBudgetCountsThisPass: a bead the drain files while clearing one
// queue line counts toward the weekly budget of the next plan in the same
// pass. Without part 1's CreatedAt stamp the next plan reads the week's filed
// count as it stood before the pass and files over the week's share
// (gt-y5wvum, gt-d75790).
func TestFollowupBudgetCountsThisPass(t *testing.T) {
	t.Parallel()
	other := ReviewFindingText{Severity: "minor", File: "internal/queue/panel.go", Line: 20,
		Title: "close the loop", Claim: "the loop never closes"}
	// Eight landings: the week's first five beads are the floor and the sixth is
	// over 25 percent. Each queue line files one bead, so with four already
	// filed the second line is over the share once it counts the first's bead.
	cases := []struct {
		name        string
		filedBefore int
		wantFiled   int
		wantQueued  int
	}{
		{"the second plan counts the first's bead and is held", 4, 1, 1},
		{"with room under the floor both lines file", 3, 2, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// Different packages, so the second line earns a bead of its own: an
			// append to the first's open bundle never counts against the budget.
			f := newFollowupFixture(t, 0.8, nil)
			for _, line := range []struct {
				bead    string
				finding ReviewFindingText
			}{{"gt-held-a", queuedFinding}, {"gt-held-b", other}} {
				if err := f.reviews.AppendQueue(queueLine(line.bead, f.work.Rig, line.finding)); err != nil {
					t.Fatal(err)
				}
				f.bd.Seed(beads.Issue{ID: line.bead, Title: "an earlier landing", Status: "closed", Type: "bug"})
			}
			// The fixture clock's week: a bead filed on its day counts against
			// eight landings an hour before it.
			week := followupDay.Add(-time.Hour)
			f.lander.Landings = &LandingsFile{Path: privateLandingsPath(t)}
			for i := 0; i < 8; i++ {
				if err := f.lander.Landings.Append(LandingRecord{BeadID: fmt.Sprintf("gt-week-%d", i), LandedAt: week}); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < c.filedBefore; i++ {
				f.fake.existing = append(f.fake.existing, ExistingFollowup{ID: fmt.Sprintf("gt-filed-week-%d", i), Title: "an older finding", Status: "open", CreatedAt: week})
			}
			log := f.run()
			if len(f.fake.created) != c.wantFiled {
				t.Fatalf("filed %d beads in one pass, want %d:\n%s", len(f.fake.created), c.wantFiled, log)
			}
			queued, err := f.reviews.Queued()
			if err != nil {
				t.Fatal(err)
			}
			if len(queued) != c.wantQueued {
				t.Fatalf("queue = %+v, want %d line(s) left", queued, c.wantQueued)
			}
			if c.wantQueued > 0 && !strings.Contains(log, "weekly budget") {
				t.Errorf("log = %q, want the second queue line held by the weekly budget", log)
			}
		})
	}
}
