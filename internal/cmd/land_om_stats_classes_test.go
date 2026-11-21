package cmd

import (
	"bytes"
	"fmt"
	"strings"
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

// TestLandOmStatsClassRecordsCountsUnparseableLines: the reader returns every
// record that decodes, whatever its attempt or verdict, skips a follow-up queue
// line and a blank line without counting them, and counts the lines it cannot
// use — one that is not JSON and one that names no instant. That count is the
// one land's own report gives for the same bytes, so the two reports agree
// about a torn log.
func TestLandOmStatsClassRecordsCountsUnparseableLines(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"bead":"gt-a","attempt":1,"verdict":"request_changes","at":"2026-11-10T10:00:00Z"}`,
		`{"bead":"gt-b","attempt":1,"verdict":"approve","at":"2026-11-11T10:00:00Z"}`,
		`{"bead":"gt-e","attempt":0,"at":"2026-11-12T10:00:00Z"}`,
		`{"bead":"gt-q","at":"2026-11-13T10:00:00Z","followup_queue":{"landed":"abc","score":0.9,"findings":[]}}`,
		`{"bead":"gt-c",`,
		`{"bead":"gt-d","attempt":1,"verdict":"approve"}`,
		`   `,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	records, unparseable := readLandOmStatsClassRecords(data)
	if len(records) != 3 || records[0].BeadID != "gt-a" || records[1].BeadID != "gt-b" || records[2].BeadID != "gt-e" {
		t.Errorf("records = %+v, want gt-a, gt-b and gt-e", records)
	}
	if unparseable != 2 {
		t.Errorf("unparseable = %d, want 2: the non-JSON line and the record with no instant", unparseable)
	}
	stats, err := land.AggregateOmStats(bytes.NewReader(data), nil, time.Time{})
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	if unparseable != stats.Skipped {
		t.Errorf("unparseable = %d, OmStats.Skipped = %d for the same bytes", unparseable, stats.Skipped)
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

// TestLandOmStatsClassesTable: two ISO weeks print as a row each — the week and
// its counts, then a cell per class: its majors and minors per 100 reviews.
func TestLandOmStatsClassesTable(t *testing.T) {
	t.Parallel()
	weeks := classRates([]land.ReviewRecord{
		landOmStatsClassReview("gt-a", 1, land.VerdictRequestChanges, time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC),
			landOmStatsClassFinding("major", "the change reaches outside the scope")),
		landOmStatsClassReview("gt-b", 1, land.VerdictRequestChanges, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
			landOmStatsClassFinding("minor", "the comment names a line number")),
	})
	var out bytes.Buffer
	if err := writeLandOmStatsClassesTable(&out, weeks); err != nil {
		t.Fatalf("writeLandOmStatsClassesTable: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	header := strings.Fields(lines[0])
	if len(lines) != 1+len(weeks) || len(header) != 3+len(weeks[0].Classes) || strings.Join(header[:3], " ") != "Week Reviews No-text" {
		t.Fatalf("table = %q, want a header of three counts and a cell per class, then a row per week", out.String())
	}
	for i, week := range weeks {
		cells := strings.Fields(lines[i+1])
		want := fmt.Sprintf("%d-W%02d %d %d", week.ISOYear, week.ISOWeek, week.Reviews, week.NoFindingText)
		if len(cells) != len(header) || strings.Join(cells[:3], " ") != want {
			t.Fatalf("row %d = %q, want %q and a cell per class", i+1, cells, want)
		}
		for j, class := range week.Classes {
			if got, want := cells[3+j], fmt.Sprintf("%.1f/%.1f", week.ratePer100(class.Majors), week.ratePer100(class.Minors)); got != want || header[3+j] != class.ID {
				t.Errorf("%d-W%02d column %q: cell %q under header %q, want %q under the class id", week.ISOYear, week.ISOWeek, class.ID, got, header[3+j], want)
			}
		}
	}
}

// TestLandOmStatsClassesTableReturnsTheWriteError: a failing writer's error is
// returned, not read as a written table.
func TestLandOmStatsClassesTableReturnsTheWriteError(t *testing.T) {
	t.Parallel()
	if err := writeLandOmStatsClassesTable(failingWriter{}, nil); err == nil {
		t.Fatal("a failed write returned no error")
	}
}
