package daemon

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/reworkstats"
)

// The rework program's weekly step (docs/design/rework-program.md, lever 6): one
// run folds the town's landing ledgers and review log into the program's two
// numbers for the last complete week and each lever's before/after deltas, and
// writes them as one JSON document under .runtime/rework-numbers. The dashboard
// feed passes the document through verbatim (internal/cmd/dashboard_rework_numbers.go,
// whose header says what reads it).
//
// Nothing schedules this step here: gt-2232t4 adds the patrol, the off-by-default
// switch and the last-run record.

const (
	// reworkNumbersFile is the document's name in its directory; each run
	// replaces that file, so a reader always reads the latest one.
	reworkNumbersFile = "latest.json"

	// reworkNumbersWeeks is the window one run reports: reworkstats' own
	// default, two months of weeks, and it must leave one complete week after
	// the week the run falls in.
	reworkNumbersWeeks = reworkstats.DefaultWeeks

	// reworkNumbersWeekLayout is the Monday form metrics.WeekOf writes and
	// reworkstats names its weeks in.
	reworkNumbersWeekLayout = "2006-01-02"

	// The reasons a figure or a lever row carries in place of a number.
	reworkNumbersStillFilling   = "the week is still filling, so it is no figure week"
	reworkNumbersOutsideWindow  = "the week is outside the window"
	reworkNumbersTooFew         = "too few beads in the week to say"
	reworkNumbersNoTimedLanding = "no landing in the week carries a polecat time"
	reworkNumbersNoBead         = "no shipping bead recorded yet"
)

// reworkNumbersStep is the weekly step's built state: the document it writes,
// the clock it reads, the window, and the reader of the town's records. The
// reader is a function so a test supplies a fake record source.
type reworkNumbersStep struct {
	out    string
	now    func() time.Time
	weeks  int
	weekOf func(string) (string, error)
	read   func() (reworkNumbersRecord, error)
}

// reworkNumbersRecord is one run's inputs: the landing records, the review
// records, and the count of lines the readers could not use.
type reworkNumbersRecord struct {
	Landings []landings.Record
	Reviews  []land.ReviewRecord
	// Unreadable is the count of landings lines and review-log lines that were
	// not records. reworkstats counts the landing records it folds into no week
	// itself, so the document's total adds this to that count.
	Unreadable int
}

// newReworkNumbersStep builds the weekly step for one town: its document path,
// its clock, and the readers of the town's landing ledgers and review log.
func newReworkNumbersStep(town string, now func() time.Time) *reworkNumbersStep {
	landingsDir := filepath.Join(constants.TownRuntimePath(town), "landings")
	return &reworkNumbersStep{
		out:    filepath.Join(constants.TownRuntimePath(town), "rework-numbers", reworkNumbersFile),
		now:    now,
		weeks:  reworkNumbersWeeks,
		weekOf: metrics.WeekOf,
		read: func() (reworkNumbersRecord, error) {
			recs, bad, err := reworkstats.ReadLandings(landingsDir)
			if err != nil {
				return reworkNumbersRecord{}, err
			}
			reviews, unreadable, err := reworkstats.ReadReviews(land.TownReviewsFile(town).Path)
			if err != nil {
				return reworkNumbersRecord{}, err
			}
			return reworkNumbersRecord{Landings: recs, Reviews: reviews, Unreadable: bad + unreadable}, nil
		},
	}
}

// run computes the document and writes it. A failed read, a report the window
// cannot give, and a failed write are the run's error, and the previous
// document stays in place: a run never publishes a document it did not finish.
func (s *reworkNumbersStep) run() error {
	in, err := s.read()
	if err != nil {
		return fmt.Errorf("reading the rework program's records: %w", err)
	}
	// One clock reading for the window and the document's own stamp, so the
	// figure week cannot disagree with the instant it was taken at.
	now := s.now()
	report, err := reworkstats.Compute(in.Landings, in.Reviews, now, s.weeks, s.weekOf)
	if err != nil {
		return fmt.Errorf("computing the rework program's numbers: %w", err)
	}
	doc, err := newReworkNumbersDocument(report, in.Landings, in.Unreadable, now, s.weekOf)
	if err != nil {
		return err
	}
	if err := atomicfile.EnsureDirAndWriteJSON(s.out, doc); err != nil {
		return fmt.Errorf("writing the rework program's document %s: %w", s.out, err)
	}
	return nil
}

// reworkNumbersDocument is the one JSON document a run writes: the last complete
// week's two numbers, one row per lever, and the record counts that say whether
// a thin week is a thin town or a lost line.
type reworkNumbersDocument struct {
	GeneratedAt time.Time                 `json:"generated_at"`
	Window      reworkNumbersRange        `json:"window"`
	Numbers     reworkNumbersFigures      `json:"numbers"`
	Levers      []reworkNumbersLever      `json:"levers"`
	Records     reworkNumbersRecordCounts `json:"records"`
}

// reworkNumbersRange is the report's window, first and last week inclusive; the
// last week is the one the run falls in and is still filling.
type reworkNumbersRange struct {
	FirstWeek string `json:"first_week"`
	LastWeek  string `json:"last_week"`
}

// reworkNumbersFigures is the program's two numbers for one week. A figure the
// week cannot give is nil, with its reason beside it, so no reader takes a
// missing share for zero.
type reworkNumbersFigures struct {
	Week               string              `json:"week"`
	Beads              int                 `json:"beads"`
	Timed              int                 `json:"timed"`
	Untimed            int                 `json:"untimed"`
	TwoRoundsShare     reworkNumbersFigure `json:"two_rounds_share"`
	MeanPolecatMinutes reworkNumbersFigure `json:"mean_polecat_minutes"`
}

// reworkNumbersFigure is one of the two numbers for one week: the value, or the
// reason the week has none.
type reworkNumbersFigure struct {
	Value  *float64 `json:"value"`
	Reason string   `json:"reason,omitempty"`
}

// reworkNumbersLever is one lever's row: the bead that shipped it, the two weeks
// its deltas are read over, and the deltas. A row the landing record cannot date
// carries a reason and no deltas.
type reworkNumbersLever struct {
	Lever                   int                 `json:"lever"`
	Bead                    string              `json:"bead,omitempty"`
	LandedWeek              string              `json:"landed_week,omitempty"`
	Before                  reworkNumbersWindow `json:"before"`
	After                   reworkNumbersWindow `json:"after"`
	TwoRoundsShareDelta     *float64            `json:"two_rounds_share_delta"`
	MeanPolecatMinutesDelta *float64            `json:"mean_polecat_minutes_delta"`
	Reason                  string              `json:"reason,omitempty"`
}

// reworkNumbersWindow is one week of a lever's before or after side: its bead
// count and the two numbers, each with its own reason when the week has none.
type reworkNumbersWindow struct {
	Week               string              `json:"week"`
	Beads              int                 `json:"beads"`
	TwoRoundsShare     reworkNumbersFigure `json:"two_rounds_share"`
	MeanPolecatMinutes reworkNumbersFigure `json:"mean_polecat_minutes"`
}

// reworkNumbersRecordCounts is what the run could not fold in: lines that were
// not records, landings outside the window, and landings with no landed_at.
type reworkNumbersRecordCounts struct {
	UnreadableLines int `json:"unreadable_lines"`
	OutsideWindow   int `json:"outside_window"`
	NoLandingTime   int `json:"no_landing_time"`
}

// reworkNumbersLeverSpec is one lever of the design doc's list: its number in
// the doc's order, the bead that shipped it, and the reason its row carries when
// the bead does not date it.
type reworkNumbersLeverSpec struct {
	lever int
	bead  string
	note  string
}

// reworkNumbersLevers is docs/design/rework-program.md's lever list in its
// order, with the bead that shipped each lever. A lever with no bead yet, and
// the lever retired before its trial, carry the note its row gives as the reason
// it has no deltas.
var reworkNumbersLevers = []reworkNumbersLeverSpec{
	{lever: 1, note: reworkNumbersNoBead},
	{lever: 2, bead: "gt-a4yodt"},
	{lever: 3, note: reworkNumbersNoBead},
	{lever: 4, note: reworkNumbersNoBead},
	{lever: 5, note: reworkNumbersNoBead},
	{lever: 6, bead: "gt-1zkwsb"},
	{lever: 7, note: "retired by gt-dpl81b"},
}

// newReworkNumbersDocument builds the document the run writes. The numbers are
// the last complete week's, and each lever is dated from the landing record of
// the bead that shipped it: the week before that landing's week and the week
// after it. A report with no complete week is an error, because a document whose
// figures are absent would read as a clean week.
func newReworkNumbersDocument(report reworkstats.Report, recs []landings.Record, unreadable int, now time.Time, weekOf func(string) (string, error)) (reworkNumbersDocument, error) {
	numbers, err := reworkNumbersFiguresOf(report)
	if err != nil {
		return reworkNumbersDocument{}, err
	}
	levers, err := reworkNumbersLeversOf(report, recs, weekOf)
	if err != nil {
		return reworkNumbersDocument{}, err
	}
	return reworkNumbersDocument{
		GeneratedAt: now,
		Window:      reworkNumbersRange{FirstWeek: report.FirstWeek, LastWeek: report.LastWeek},
		Numbers:     numbers,
		Levers:      levers,
		Records: reworkNumbersRecordCounts{
			UnreadableLines: report.Skipped + unreadable,
			OutsideWindow:   report.Outside,
			NoLandingTime:   report.NoLandingTime,
		},
	}, nil
}

// reworkNumbersFiguresOf takes the report's last complete week. The report's
// last week is the week the run falls in and is still filling, so it is never a
// figure week.
func reworkNumbersFiguresOf(report reworkstats.Report) (reworkNumbersFigures, error) {
	if len(report.Weeks) < 2 {
		return reworkNumbersFigures{}, fmt.Errorf("the window %s to %s holds no complete week: its last week is the week the run falls in", report.FirstWeek, report.LastWeek)
	}
	week := report.Weeks[len(report.Weeks)-2]
	f := reworkNumbersFigures{Week: week.Week, Beads: week.Beads, Timed: week.Timed, Untimed: week.Untimed}
	if week.TooFew {
		f.TwoRoundsShare.Reason = reworkNumbersTooFew
		f.MeanPolecatMinutes.Reason = reworkNumbersTooFew
		return f, nil
	}
	f.TwoRoundsShare.Value = week.TwoRoundsShare
	f.MeanPolecatMinutes.Value = week.MeanPolecatMinutes
	if week.MeanPolecatMinutes == nil {
		f.MeanPolecatMinutes.Reason = reworkNumbersNoTimedLanding
	}
	return f, nil
}

// reworkNumbersLeversOf dates every lever and reads its two windows. A week the
// report cannot name is an error; a lever the records cannot date is a row with
// a reason, so one unlanded lever never costs the other rows.
func reworkNumbersLeversOf(report reworkstats.Report, recs []landings.Record, weekOf func(string) (string, error)) ([]reworkNumbersLever, error) {
	rows := make([]reworkNumbersLever, 0, len(reworkNumbersLevers))
	for _, spec := range reworkNumbersLevers {
		row := reworkNumbersLever{Lever: spec.lever, Bead: spec.bead, Reason: spec.note}
		if spec.bead == "" {
			rows = append(rows, row)
			continue
		}
		landed, ok := reworkNumbersLeverLanding(spec.bead, recs)
		if !ok {
			row.Reason = "no landing record for " + spec.bead
			rows = append(rows, row)
			continue
		}
		if landed.LandedAt.IsZero() {
			row.Reason = "the landing record for " + spec.bead + " has no landed_at"
			rows = append(rows, row)
			continue
		}
		week, err := weekOf(landed.LandedAt.Format(time.RFC3339Nano))
		if err != nil {
			return nil, fmt.Errorf("the week of %s's landing: %w", spec.bead, err)
		}
		// One parse of the week dates both windows, so the before and after
		// sides cannot disagree about the day the landing week starts.
		start, err := time.ParseInLocation(reworkNumbersWeekLayout, week, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("the week of %s's landing, %q: %w", spec.bead, week, err)
		}
		row.LandedWeek = week
		row.Before = reworkNumbersWindowOf(report, start.AddDate(0, 0, -7).Format(reworkNumbersWeekLayout))
		row.After = reworkNumbersWindowOf(report, start.AddDate(0, 0, 7).Format(reworkNumbersWeekLayout))
		row.TwoRoundsShareDelta = reworkNumbersDelta(row.Before.TwoRoundsShare.Value, row.After.TwoRoundsShare.Value)
		row.MeanPolecatMinutesDelta = reworkNumbersDelta(row.Before.MeanPolecatMinutes.Value, row.After.MeanPolecatMinutes.Value)
		rows = append(rows, row)
	}
	return rows, nil
}

// reworkNumbersLeverLanding is the latest landing record for bead: a bead
// landed more than once is dated from its last landing, as the numbers count it.
func reworkNumbersLeverLanding(bead string, recs []landings.Record) (landings.Record, bool) {
	var best landings.Record
	found := false
	for _, rec := range recs {
		if rec.Bead != bead {
			continue
		}
		if !found || rec.LandedAt.After(best.LandedAt) {
			best, found = rec, true
		}
	}
	return best, found
}

// reworkNumbersWindowOf reads one week of a lever's row. A week the report does
// not hold, the week the run falls in, and a week too thin to give a figure each
// leave the two figures nil with that reason.
func reworkNumbersWindowOf(report reworkstats.Report, week string) reworkNumbersWindow {
	w := reworkNumbersWindow{Week: week}
	row, inWindow := reworkNumbersWeekRow(report, week)
	if !inWindow {
		w.TwoRoundsShare.Reason = reworkNumbersOutsideWindow
		w.MeanPolecatMinutes.Reason = reworkNumbersOutsideWindow
		return w
	}
	w.Beads = row.Beads
	switch {
	case week == report.LastWeek:
		w.TwoRoundsShare.Reason = reworkNumbersStillFilling
		w.MeanPolecatMinutes.Reason = reworkNumbersStillFilling
	case row.TooFew:
		w.TwoRoundsShare.Reason = reworkNumbersTooFew
		w.MeanPolecatMinutes.Reason = reworkNumbersTooFew
	default:
		w.TwoRoundsShare.Value = row.TwoRoundsShare
		w.MeanPolecatMinutes.Value = row.MeanPolecatMinutes
		if row.MeanPolecatMinutes == nil {
			w.MeanPolecatMinutes.Reason = reworkNumbersNoTimedLanding
		}
	}
	return w
}

// reworkNumbersWeekRow finds one week's row in the report.
func reworkNumbersWeekRow(report reworkstats.Report, week string) (reworkstats.Week, bool) {
	for _, row := range report.Weeks {
		if row.Week == week {
			return row, true
		}
	}
	return reworkstats.Week{}, false
}

// reworkNumbersDelta is after minus before, nil when either side gave no
// figure: a delta over a missing week would read as a change.
func reworkNumbersDelta(before, after *float64) *float64 {
	if before == nil || after == nil {
		return nil
	}
	d := *after - *before
	return &d
}
