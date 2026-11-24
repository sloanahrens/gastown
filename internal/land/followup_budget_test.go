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
		{"8 of 32 is exactly at 25 percent so the ninth is refused", 8, 32, 25, false},
		{"zero percent is the 25 default", 24, 100, 0, true},
		{"zero percent refuses what 25 percent refuses", 25, 100, 0, false},
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
	noStamp := ExistingFollowup{ID: "gt-undated"}
	cases := []struct {
		name                   string
		existing               []ExistingFollowup
		since                  time.Time
		wantFiled, wantUndated int
	}{
		{"beads at or after the cutoff count and the undated one is reported", []ExistingFollowup{
			{ID: "gt-before", CreatedAt: cutoff.Add(-time.Hour)},
			{ID: "gt-at", CreatedAt: cutoff},
			{ID: "gt-after", CreatedAt: cutoff.Add(time.Minute)},
			noStamp,
		}, cutoff, 2, 1},
		{"the undated bead is reported from the zero cutoff too", []ExistingFollowup{{ID: "gt-dated", CreatedAt: cutoff}, noStamp}, time.Time{}, 1, 1},
		{"nothing is reported when every bead has a stamp", []ExistingFollowup{{ID: "gt-at", CreatedAt: cutoff}}, cutoff, 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filed, undated := FiledSince(c.existing, c.since)
			if filed != c.wantFiled || undated != c.wantUndated {
				t.Errorf("FiledSince(%d bead(s), %v) = %d filed, %d undated; want %d and %d",
					len(c.existing), c.since, filed, undated, c.wantFiled, c.wantUndated)
			}
		})
	}
}

// TestFollowupBudgetReportsAnUndatedBead: the feed says how many beads it could
// not date, so a filed stamp that did not parse is not read as a bead that was
// never filed (gt-ytgo9c).
func TestFollowupBudgetReportsAnUndatedBead(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	f.fake.existing = []ExistingFollowup{{ID: "gt-undated", Title: "an older finding", Status: "open"}}
	log := f.run()
	if !strings.Contains(log, "no readable filed stamp") {
		t.Errorf("log = %q, want the undated bead reported", log)
	}
}

// TestFollowupBudgetIgnoresTheUndatedBeadInTheCount: a bead with no readable
// filed stamp is left out of the week's count, so one parse failure is not a
// cut to every later week's budget (gt-ytgo9c). Twenty-four dated beads and 100
// landings leave room for one more bead at 25 percent; counting the undated bead
// as filed too would hold it.
func TestFollowupBudgetIgnoresTheUndatedBeadInTheCount(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	f.lander.Landings = budgetWeek(t, 100)
	f.fake.existing = append(filedThisWeek(24), ExistingFollowup{ID: "gt-undated", Title: "an older finding", Status: "open"})
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads with 24 dated and one undated in the week, want 1:\n%s", len(f.fake.created), log)
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
