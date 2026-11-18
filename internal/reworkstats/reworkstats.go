// Package reworkstats computes the rework program's weekly figures — the share
// of landed beads that needed two or more om review rounds, and the mean
// polecat minutes per landed bead — from the town's landing and review records.
package reworkstats

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
)

const (
	// DefaultWeeks is the window `gt land rework-stats` reports without
	// --weeks: two months of weeks, long enough for a lever to have before
	// and after windows of equal length.
	DefaultWeeks = 8
	// minBeads is the smallest week worth a share or a mean. A thinner week is
	// marked too few to say so a change cannot be read off a handful of beads.
	minBeads = 20
	// weekLayout is the Monday form metrics.WeekOf writes and the report steps
	// by.
	weekLayout = "2006-01-02"
)

// Report is one window's weekly figures. Shares are fractions of a week's
// Beads, the mean is minutes, and a nil figure is one the week cannot give —
// too few beads, or no timed landing — so a reader never takes it for zero.
type Report struct {
	FirstWeek string `json:"first_week"`
	LastWeek  string `json:"last_week"`
	Weeks     []Week `json:"weeks"`
	// Outside counts the landing records whose week is not in the window.
	Outside int `json:"outside_window"`
	// NoLandingTime counts the landing records with no landed_at: a week is
	// read from that field, so such a record folds into no week.
	NoLandingTime int `json:"no_landing_time"`
	// Skipped counts the records that fold into no week: a landing that names
	// no bead, plus any unparsable lines the caller's readers add here, so the
	// total a caller reports is every line the run could not use.
	Skipped int `json:"unreadable_lines"`
}

// Week is one week's numbers. TooFew says the week holds fewer than minBeads,
// which leaves its shares and mean nil.
type Week struct {
	Week           string   `json:"week"`
	Beads          int      `json:"beads"`
	TwoRounds      int      `json:"two_rounds"`
	TwoRoundsShare *float64 `json:"two_rounds_share"`
	TwoSeats       int      `json:"two_seats"`
	TwoSeatsShare  *float64 `json:"two_seats_share"`
	// Timed is the beads whose landing record carried a polecat time, and
	// Untimed the ones it did not. The mean is over Timed alone.
	Timed              int      `json:"timed"`
	Untimed            int      `json:"untimed"`
	MeanPolecatMinutes *float64 `json:"mean_polecat_minutes"`
	TooFew             bool     `json:"too_few_to_say,omitempty"`
}

// Compute folds the landing records and review records into the weeks ending
// with the one now falls in.
//
// A bead's rounds are the highest attempt its review records carry, joined by
// its id, which is unique town-wide and the key every other reader of these two
// files joins on; its seats are the polecat attempts its landing carries. A
// bead landed more than once counts once, in the week of its last landing, and
// a landing with no polecat time counts in Untimed and stays out of the mean.
//
// weekOf names a timestamp's Monday; the command passes metrics.WeekOf, and a
// test passes one that fails, to drive each refusal. A week the function cannot
// name, and a window shorter than one week, fail the whole report: a partial or
// empty one would read as a clean week.
func Compute(recs []landings.Record, reviews []land.ReviewRecord, now time.Time, weeks int, weekOf func(string) (string, error)) (Report, error) {
	if weeks < 1 {
		return Report{}, fmt.Errorf("--weeks %d: want at least 1", weeks)
	}
	lastWeek, err := weekOf(now.Format(time.RFC3339Nano))
	if err != nil {
		return Report{}, fmt.Errorf("the week of %s: %w", now.Format(time.RFC3339), err)
	}
	// Subtracting whole weeks keeps the weekday, so its week is the one weeks-1
	// before the last.
	firstWeek, err := weekOf(now.AddDate(0, 0, -7*(weeks-1)).Format(time.RFC3339Nano))
	if err != nil {
		return Report{}, fmt.Errorf("the week of the window's first day: %w", err)
	}
	names, err := weekNames(firstWeek, weeks)
	if err != nil {
		return Report{}, err
	}
	report := Report{FirstWeek: firstWeek, LastWeek: lastWeek}

	// A bead landed more than once counts once, in the week of its last
	// landing: the program's figure is beads, not landings.
	latest := map[string]landings.Record{}
	for _, rec := range recs {
		if strings.TrimSpace(rec.Bead) == "" {
			report.Skipped++
			continue
		}
		if rec.LandedAt.IsZero() {
			report.NoLandingTime++
			continue
		}
		old, ok := latest[rec.Bead]
		if !ok || rec.LandedAt.After(old.LandedAt) {
			latest[rec.Bead] = rec
		}
	}

	rounds := map[string]int{}
	for _, rec := range reviews {
		if rec.BeadID == "" {
			continue
		}
		if rec.Attempt > rounds[rec.BeadID] {
			rounds[rec.BeadID] = rec.Attempt
		}
	}

	type tally struct {
		beads, twoRounds, twoSeats, timed int
		secs                              float64
	}
	byWeek := map[string]*tally{}
	for _, rec := range latest {
		week, err := weekOf(rec.LandedAt.Format(time.RFC3339Nano))
		if err != nil {
			return Report{}, fmt.Errorf("the week of %s's landing: %w", rec.Bead, err)
		}
		// The names carry one format, so they sort as the days they name.
		if week < firstWeek || week > lastWeek {
			report.Outside++
			continue
		}
		t := byWeek[week]
		if t == nil {
			t = &tally{}
			byWeek[week] = t
		}
		t.beads++
		if rounds[rec.Bead] >= 2 {
			t.twoRounds++
		}
		if len(rec.PolecatAttempts) >= 2 {
			t.twoSeats++
		}
		if rec.PolecatSecs > 0 {
			t.timed++
			t.secs += rec.PolecatSecs
		}
	}

	report.Weeks = make([]Week, 0, len(names))
	for _, week := range names {
		row := Week{Week: week}
		if t := byWeek[week]; t != nil {
			row.Beads, row.TwoRounds, row.TwoSeats = t.beads, t.twoRounds, t.twoSeats
			row.Timed = t.timed
			row.Untimed = t.beads - t.timed
			if t.beads >= minBeads {
				twoRounds := float64(t.twoRounds) / float64(t.beads)
				twoSeats := float64(t.twoSeats) / float64(t.beads)
				row.TwoRoundsShare, row.TwoSeatsShare = &twoRounds, &twoSeats
				// A week whose every landing is untimed has no mean to give,
				// and zero would read as a fast week.
				if t.timed > 0 {
					mean := t.secs / float64(t.timed) / 60
					row.MeanPolecatMinutes = &mean
				}
			} else {
				row.TooFew = true
			}
		} else {
			row.TooFew = true
		}
		report.Weeks = append(report.Weeks, row)
	}
	return report, nil
}

// weekNames lists the weeks from first, oldest first.
func weekNames(first string, weeks int) ([]string, error) {
	start, err := time.ParseInLocation(weekLayout, first, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("the window's first week %q: %w", first, err)
	}
	names := make([]string, 0, weeks)
	for i := 0; i < weeks; i++ {
		names = append(names, start.AddDate(0, 0, 7*i).Format(weekLayout))
	}
	return names, nil
}
