package cmd

// The class-rate aggregation behind `gt land om-stats --classes` (gt-sm3wgg):
// how often each class of the om-lessons table turns up in the town's reviews,
// week by week, so a class that stops recurring after its fix landed shows
// without a hand analysis.

import (
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// landOmStatsClassWeek is one ISO week of the class-rate table, with a counts
// row for every class of the om-lessons table: a class the week never saw reads
// as zeros rather than as a missing column. ISOYear and ISOWeek are the week's
// ISO 8601 year and number, of the reviews' times in UTC, so the week runs
// Monday to Sunday throughout: 2026-01-01 belongs to ISO 2026-W01, whose Monday
// is 2025-12-29.
type landOmStatsClassWeek struct {
	ISOYear int `json:"iso_year"`
	ISOWeek int `json:"iso_week"`
	// Reviews counts the records that judged a tree; a review with no finding
	// text stays in this count and in every rate's denominator.
	Reviews int `json:"reviews"`
	// NoFindingText counts those reviews that carry no finding text, whose
	// findings are in no class.
	NoFindingText int `json:"no_finding_text"`
	// Classes is in class-id order, so any two weeks compare column by column.
	Classes []landOmStatsClassCount `json:"classes"`
}

// landOmStatsClassCount is one class's findings in one week: major counts
// critical and major findings, minor counts minor and info findings.
type landOmStatsClassCount struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Majors int    `json:"majors"`
	Minors int    `json:"minors"`
}

// ratePer100 returns n findings per 100 reviews of the week, the figure a cell
// of the table shows; a week with no reviews has no rate and returns 0.
func (w landOmStatsClassWeek) ratePer100(n int) float64 {
	if w.Reviews <= 0 {
		return 0
	}
	return float64(n) * 100 / float64(w.Reviews)
}

// classRates aggregates the review records into the table's rows, one per ISO
// week, in week order.
//
// A record that is not a review counts nowhere: not in a week, a no-text total
// or a class. A finding joins the class omlessons.Classify names; a critical or
// major severity counts as major, a minor or info one as minor, padded or
// shifted in case as om writes it, and any other severity counts in neither, as
// land's own readers count it as nothing (gt-oi4je). A finding no class matches
// counts in no class, and `gt land om-lessons` lists those titles.
func classRates(records []land.ReviewRecord) []landOmStatsClassWeek {
	names := omlessons.ClassNames()
	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tallies := map[time.Time]*landOmStatsWeekTally{}
	for _, rec := range records {
		// A follow-up queue line is the feed's bookkeeping rather than a
		// verdict (gt-3arxj); a record with no attempt or no verdict judged
		// nothing, and a review that judged nothing has no log line at all
		// (gt-mctxx). A line with no instant names no week to count in.
		if rec.Queue != nil || rec.At.IsZero() || rec.Attempt <= 0 || strings.TrimSpace(rec.Verdict) == "" {
			continue
		}
		monday := landOmStatsMondayUTC(rec.At)
		if tallies[monday] == nil {
			tallies[monday] = &landOmStatsWeekTally{counts: map[string]landOmStatsSeverityCounts{}}
		}
		tallies[monday].add(rec)
	}

	mondays := make([]time.Time, 0, len(tallies))
	for monday := range tallies {
		mondays = append(mondays, monday)
	}
	sort.Slice(mondays, func(i, j int) bool { return mondays[i].Before(mondays[j]) })
	out := make([]landOmStatsClassWeek, 0, len(mondays))
	for _, monday := range mondays {
		out = append(out, tallies[monday].row(monday, ids, names))
	}
	return out
}

// landOmStatsMondayUTC returns the midnight-UTC Monday of the ISO week at falls
// in, its time taken in UTC: a time near midnight belongs to the week UTC puts
// it in, not the week its own zone shows.
func landOmStatsMondayUTC(at time.Time) time.Time {
	t := at.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
}

type landOmStatsSeverityCounts struct{ majors, minors int }

// landOmStatsWeekTally is one week's figures while classRates builds them.
type landOmStatsWeekTally struct {
	reviews       int
	noFindingText int
	counts        map[string]landOmStatsSeverityCounts
}

func (t *landOmStatsWeekTally) add(rec land.ReviewRecord) {
	t.reviews++
	if len(rec.FindingText) == 0 {
		t.noFindingText++
		return
	}
	for _, f := range rec.FindingText {
		id := omlessons.Classify(f)
		if id == omlessons.Unclassified {
			continue
		}
		c := t.counts[id]
		switch strings.ToLower(strings.TrimSpace(f.Severity)) {
		case "critical", "major":
			c.majors++
		case "minor", "info":
			c.minors++
		}
		t.counts[id] = c
	}
}

// row renders the tally as the week's table row.
func (t *landOmStatsWeekTally) row(monday time.Time, ids []string, names map[string]string) landOmStatsClassWeek {
	classes := make([]landOmStatsClassCount, 0, len(ids))
	for _, id := range ids {
		c := t.counts[id]
		classes = append(classes, landOmStatsClassCount{ID: id, Name: names[id], Majors: c.majors, Minors: c.minors})
	}
	year, week := monday.ISOWeek()
	return landOmStatsClassWeek{
		ISOYear: year, ISOWeek: week,
		Reviews: t.reviews, NoFindingText: t.noFindingText, Classes: classes,
	}
}
