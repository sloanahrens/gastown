package land

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	f := &LandingsFile{Path: filepath.Join(t.TempDir(), "landings.jsonl")}
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
