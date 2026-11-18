package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

func landOmStatsClassFinding(severity, title string) land.ReviewFindingText {
	return land.ReviewFindingText{Severity: severity, Title: title}
}

func landOmStatsClassReview(bead string, attempt int, verdict string, at time.Time, findings ...land.ReviewFindingText) land.ReviewRecord {
	return land.ReviewRecord{BeadID: bead, Attempt: attempt, Verdict: verdict, At: at, FindingText: findings}
}

// landOmStatsClassCountFor returns one week's counts row for a class.
func landOmStatsClassCountFor(t *testing.T, week landOmStatsClassWeek, id string) landOmStatsClassCount {
	t.Helper()
	for _, c := range week.Classes {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("ISO week %d has no class row %q", week.ISOWeek, id)
	return landOmStatsClassCount{}
}

// landOmStatsClassAssertNoFindings fails if any class row counts a finding.
func landOmStatsClassAssertNoFindings(t *testing.T, week landOmStatsClassWeek) {
	t.Helper()
	for _, c := range week.Classes {
		if c.Majors != 0 || c.Minors != 0 {
			t.Errorf("class %s counts majors %d minors %d, want none", c.ID, c.Majors, c.Minors)
		}
	}
}

// TestLandOmStatsClassRates: two ISO weeks of reviews, each asserting its review
// count and its majors, minors and per-100-review rate per class. The gt-c
// record is Sunday 23:30 UTC written in a +01:00 zone, so it stays in week 46
// only because the week is taken in UTC: its own zone makes it Monday of week
// 47.
func TestLandOmStatsClassRates(t *testing.T) {
	t.Parallel()
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC) }
	records := []land.ReviewRecord{
		landOmStatsClassReview("gt-a", 1, land.VerdictApprove, at(9, 8),
			landOmStatsClassFinding("minor", "the comment repeats the flag list")),
		landOmStatsClassReview("gt-b", 1, land.VerdictRequestChanges, at(10, 10),
			landOmStatsClassFinding("major", "the change reaches outside the scope"),
			landOmStatsClassFinding("minor", "the comment names a line number")),
		landOmStatsClassReview("gt-b", 2, land.VerdictRequestChanges, at(11, 10),
			landOmStatsClassFinding("critical", "the comment misstates the bound"),
			landOmStatsClassFinding("minor", "the change reaches outside the scope")),
		landOmStatsClassReview("gt-c", 1, land.VerdictRequestChanges,
			time.Date(2026, 11, 16, 0, 30, 0, 0, time.FixedZone("CET", 3600)),
			landOmStatsClassFinding("major", "the change reaches outside the scope")),
		landOmStatsClassReview("gt-d", 1, land.VerdictApprove, at(17, 10),
			landOmStatsClassFinding("info", "the comment says the caller handles nil")),
		landOmStatsClassReview("gt-e", 3, land.VerdictRequestChanges, at(18, 10),
			landOmStatsClassFinding("major", "the change reaches outside the scope")),
	}
	weeks := classRates(records)
	if len(weeks) != 2 {
		t.Fatalf("weeks = %d, want 2", len(weeks))
	}
	w46, w47 := weeks[0], weeks[1]
	if w46.ISOYear != 2026 || w46.ISOWeek != 46 || w46.Reviews != 4 {
		t.Errorf("first week = %d-W%d with %d reviews, want 2026-W46 with 4", w46.ISOYear, w46.ISOWeek, w46.Reviews)
	}
	if w47.ISOYear != 2026 || w47.ISOWeek != 47 || w47.Reviews != 2 {
		t.Errorf("second week = %d-W%d with %d reviews, want 2026-W47 with 2", w47.ISOYear, w47.ISOWeek, w47.Reviews)
	}
	if len(w46.Classes) != len(omlessons.ClassNames()) {
		t.Errorf("week has %d class rows, want one per class of the table (%d)", len(w46.Classes), len(omlessons.ClassNames()))
	}
	cases := []struct {
		name           string
		week           landOmStatsClassWeek
		id             string
		majors, minors int
		majorRate      float64
		minorRate      float64
	}{
		{"week46 scope-widened", w46, "scope-widened", 2, 1, 50, 25},
		{"week46 comment-claim", w46, "comment-claim", 1, 2, 25, 50},
		{"week46 secret-in-output", w46, "secret-in-output", 0, 0, 0, 0},
		{"week47 scope-widened", w47, "scope-widened", 1, 0, 50, 0},
		{"week47 comment-claim", w47, "comment-claim", 0, 1, 0, 50},
	}
	for _, c := range cases {
		got := landOmStatsClassCountFor(t, c.week, c.id)
		if got.Majors != c.majors || got.Minors != c.minors {
			t.Errorf("%s: majors %d minors %d, want majors %d minors %d", c.name, got.Majors, got.Minors, c.majors, c.minors)
		}
		if rate := c.week.ratePer100(got.Majors); rate != c.majorRate {
			t.Errorf("%s: major rate %v per 100 reviews, want %v", c.name, rate, c.majorRate)
		}
		if rate := c.week.ratePer100(got.Minors); rate != c.minorRate {
			t.Errorf("%s: minor rate %v per 100 reviews, want %v", c.name, rate, c.minorRate)
		}
	}
}

// TestLandOmStatsClassRatesSkipsNonReviews: a review with no finding text is
// counted as a review and in the no-text total, and in no class; a record with
// no attempt, one with no verdict, one with no instant, and a follow-up queue
// line are not reviews and count nowhere, whatever findings they carry.
func TestLandOmStatsClassRatesSkipsNonReviews(t *testing.T) {
	t.Parallel()
	at := func(day int) time.Time { return time.Date(2026, 11, day, 10, 0, 0, 0, time.UTC) }
	records := []land.ReviewRecord{
		{BeadID: "gt-a", Attempt: 2, Verdict: land.VerdictApprove, At: at(10)},
		landOmStatsClassReview("gt-b", 0, land.VerdictRequestChanges, at(11),
			landOmStatsClassFinding("major", "the change reaches outside the scope")),
		landOmStatsClassReview("gt-c", 1, "", at(12),
			landOmStatsClassFinding("major", "the change reaches outside the scope")),
		landOmStatsClassReview("gt-d", 1, land.VerdictApprove, time.Time{},
			landOmStatsClassFinding("major", "the change reaches outside the scope")),
		{BeadID: "gt-e", Attempt: 1, Verdict: land.VerdictApprove, At: at(19), Queue: &land.FollowupQueue{}},
	}
	weeks := classRates(records)
	if len(weeks) != 1 {
		t.Fatalf("weeks = %d, want 1: the non-review records count nowhere", len(weeks))
	}
	week := weeks[0]
	if week.Reviews != 1 || week.NoFindingText != 1 {
		t.Errorf("reviews %d no-text %d, want 1 and 1", week.Reviews, week.NoFindingText)
	}
	landOmStatsClassAssertNoFindings(t, week)
}

// TestLandOmStatsClassRatesCountsNoFindingInNoClass: a finding of an unknown
// severity counts in neither column, and a finding no class matches counts in no
// class, while the review that carried it still counts.
func TestLandOmStatsClassRatesCountsNoFindingInNoClass(t *testing.T) {
	t.Parallel()
	cases := map[string]land.ReviewFindingText{
		"unknown severity": landOmStatsClassFinding("blocker", "the change reaches outside the scope"),
		"no class matches": landOmStatsClassFinding("major", "the retry loop never resets its counter"),
	}
	for name, finding := range cases {
		t.Run(name, func(t *testing.T) {
			record := landOmStatsClassReview("gt-a", 1, land.VerdictRequestChanges,
				time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC), finding)
			weeks := classRates([]land.ReviewRecord{record})
			if len(weeks) != 1 || weeks[0].Reviews != 1 || weeks[0].NoFindingText != 0 {
				t.Fatalf("weeks = %+v, want one week with one review carrying text", weeks)
			}
			landOmStatsClassAssertNoFindings(t, weeks[0])
		})
	}
}

// TestLandOmStatsClassRatesNoReviewsHasNoRate: a week with no reviews returns a
// rate of 0 rather than dividing by zero.
func TestLandOmStatsClassRatesNoReviewsHasNoRate(t *testing.T) {
	t.Parallel()
	if rate := (landOmStatsClassWeek{}).ratePer100(4); rate != 0 {
		t.Errorf("rate = %v for a week with no reviews, want 0", rate)
	}
}
