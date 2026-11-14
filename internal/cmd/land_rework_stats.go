package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

const (
	// landReworkStatsDefaultWeeks is the window `gt land rework-stats` reports
	// without --weeks: two months of weeks, long enough for a lever to have
	// before and after windows of equal length.
	landReworkStatsDefaultWeeks = 8
	// landReworkStatsMinBeads is the smallest week worth a share or a mean. A
	// thinner week is printed "too few to say" so a change cannot be read off
	// a handful of beads.
	landReworkStatsMinBeads = 20
	// landReworkStatsWeekLayout is the Monday form metrics.WeekOf writes and
	// this report steps by.
	landReworkStatsWeekLayout = "2006-01-02"
)

var (
	landReworkStatsWeeks int
	landReworkStatsJSON  bool
)

// landReworkStatsCmd prints the two figures the rework program is judged on,
// week by week, from the landing record. It reads and writes nothing.
var landReworkStatsCmd = &cobra.Command{
	Use:   "rework-stats",
	Short: "Report the rework program's weekly rework share and polecat minutes",
	Long: `Report the rework program's two numbers, week by week.

Lever 6 of the rework program (docs/design/rework-program.md) is judged on two
figures from the landing record: the share of landed beads that needed two or
more om review rounds, and the mean polecat minutes per landed bead. The report
prints both per week, beside the share that needed two or more polecat seats and
the week's bead count.

Reads the rig landing ledgers (.runtime/landings/<rig>.jsonl) and the review log
(.runtime/landings/reviews.jsonl), the files the other gt land commands read. A
bead's rounds are the highest attempt its review records carry; its seats are
the polecat_attempts its landing record carries. A landing recorded with no
polecat time is counted in the No time column and left out of the mean, never
read as zero. A bead landed twice counts once, in the week of its last landing.

A week with fewer than 20 beads prints "too few to say": the program's figure is
not judged on fewer beads than that.

The command reads only: it writes no file. --json prints the same numbers.

Examples:
  gt land rework-stats
  gt land rework-stats --weeks 12
  gt land rework-stats --json`,
	Args: cobra.NoArgs,
	RunE: runLandReworkStats,
}

func init() {
	landReworkStatsCmd.Flags().IntVar(&landReworkStatsWeeks, "weeks", landReworkStatsDefaultWeeks, "weeks to report, ending with the current one")
	landReworkStatsCmd.Flags().BoolVar(&landReworkStatsJSON, "json", false, "print the report as JSON")
	landCmd.AddCommand(landReworkStatsCmd)
}

func runLandReworkStats(cmd *cobra.Command, _ []string) error {
	return landReworkStatsReportFrom(workspace.FindFromCwdOrError, cmd.OutOrStdout(), time.Now(), landReworkStatsWeeks, landReworkStatsJSON)
}

// landReworkReport is the whole report. Shares are fractions of a week's Beads,
// the mean is minutes, and a nil figure is one the week cannot give — too few
// beads, or no timed landing — so a reader never takes it for zero.
type landReworkReport struct {
	FirstWeek string           `json:"first_week"`
	LastWeek  string           `json:"last_week"`
	Weeks     []landReworkWeek `json:"weeks"`
	// Outside counts the landing records whose week is not in the window.
	Outside int `json:"outside_window"`
	// NoLandingTime counts the landing records with no landed_at: a week is
	// read from that field, so such a record folds into no week.
	NoLandingTime int `json:"no_landing_time"`
	// Skipped counts the lines of the two logs the report could not use: one
	// that does not parse, and a landing that names no bead.
	Skipped int `json:"unreadable_lines"`
}

// landReworkWeek is one week's numbers. TooFew says the week is thinner than
// landReworkStatsMinBeads, which leaves its shares and mean nil.
type landReworkWeek struct {
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

// landReworkStatsReportFrom is the command body over explicitly named inputs,
// so a test supplies the town and the clock without swapping the shared flag
// variables.
func landReworkStatsReportFrom(findTown func() (string, error), out io.Writer, now time.Time, weeks int, asJSON bool) error {
	report, err := landReworkStatsFromTown(findTown, now, weeks)
	if err != nil {
		return err
	}
	if asJSON {
		return writeLandReworkStatsJSON(out, report)
	}
	return writeLandReworkStatsTable(out, report)
}

// landReworkStatsFromTown reads the town's landing ledgers and review log and
// folds them into the report.
func landReworkStatsFromTown(findTown func() (string, error), now time.Time, weeks int) (landReworkReport, error) {
	if weeks < 1 {
		return landReworkReport{}, fmt.Errorf("--weeks %d: want at least 1", weeks)
	}
	town, err := findTown()
	if err != nil {
		return landReworkReport{}, fmt.Errorf("resolving the town: %w", err)
	}
	dir := landReworkStatsLandingsDir(town)
	recs, skipped, err := readLandReworkStatsLandings(dir)
	if err != nil {
		return landReworkReport{}, err
	}
	reviews, unreadable, err := readLandReworkStatsReviews(land.TownReviewsFile(town).Path)
	if err != nil {
		return landReworkReport{}, err
	}
	report, err := landReworkStatsAggregate(recs, reviews, now, weeks, metrics.WeekOf)
	if err != nil {
		return landReworkReport{}, err
	}
	report.Skipped += skipped + unreadable
	return report, nil
}

// landReworkStatsLandingsDir is where the landing worker keeps every rig's
// ledger and the town's review log.
func landReworkStatsLandingsDir(town string) string {
	return filepath.Join(constants.TownRuntimePath(town), "landings")
}

// readLandReworkStatsLandings reads every rig's landing ledger in dir. A
// directory that does not exist is a town that has not landed yet, and no
// records; a file that will not open or read is an error naming it, so a
// report never reads a failed read as an empty week.
func readLandReworkStatsLandings(dir string) ([]landings.Record, int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading the landings directory %s: %w", dir, err)
	}
	var recs []landings.Record
	skipped := 0
	for _, entry := range entries {
		name := entry.Name()
		// The review log is a .jsonl in the same directory and is not a ledger;
		// the verdict store and the backoff snapshots are directories and other
		// file kinds.
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") || name == land.ReviewsFileName {
			continue
		}
		got, bad, err := (&landings.Reader{Path: filepath.Join(dir, name)}).ReadNew()
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", name, err)
		}
		recs = append(recs, got...)
		skipped += len(bad)
	}
	return recs, skipped, nil
}

// readLandReworkStatsReviews reads the town's om verdicts through the review
// log's own line scanner, skipping the queue lines that are not verdicts and
// counting the lines it cannot use, because a lost verdict reads as a bead that
// needed fewer rounds. A log that does not exist is a town whose landings were
// never reviewed, and no records.
func readLandReworkStatsReviews(path string) ([]land.ReviewRecord, int, error) {
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("opening the review records %s: %w", path, err)
	}
	defer func() { _ = fh.Close() }()
	var recs []land.ReviewRecord
	unreadable := 0
	_, err = land.ScanReviewLines(fh, func(raw []byte) {
		// The scanner hands a blank final line to every reader of a file that
		// ends in a newline; a blank line is no record, not an unreadable one.
		if len(raw) == 0 || land.IsQueueLine(raw) {
			return
		}
		var rec land.ReviewRecord
		if json.Unmarshal(raw, &rec) != nil {
			unreadable++
			return
		}
		recs = append(recs, rec)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("reading the review records %s: %w", path, err)
	}
	return recs, unreadable, nil
}

// landReworkStatsAggregate folds the landing records and review records into
// the weeks ending with the one now falls in. The rounds of a bead are joined
// by its id, which is unique town-wide and the key every other reader of these
// two files joins on. weekOf names a timestamp's Monday; the report passes
// metrics.WeekOf, and a test passes one that fails, to drive each refusal.
func landReworkStatsAggregate(recs []landings.Record, reviews []land.ReviewRecord, now time.Time, weeks int, weekOf func(string) (string, error)) (landReworkReport, error) {
	lastWeek, err := weekOf(now.Format(time.RFC3339Nano))
	if err != nil {
		return landReworkReport{}, fmt.Errorf("the week of %s: %w", now.Format(time.RFC3339), err)
	}
	// Subtracting whole weeks keeps the weekday, so its week is the one weeks-1
	// before the last.
	firstWeek, err := weekOf(now.AddDate(0, 0, -7*(weeks-1)).Format(time.RFC3339Nano))
	if err != nil {
		return landReworkReport{}, fmt.Errorf("the week of the window's first day: %w", err)
	}
	weekNames, err := landReworkStatsWeekNames(firstWeek, weeks)
	if err != nil {
		return landReworkReport{}, err
	}
	report := landReworkReport{FirstWeek: firstWeek, LastWeek: lastWeek}

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
			return landReworkReport{}, fmt.Errorf("the week of %s's landing: %w", rec.Bead, err)
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

	report.Weeks = make([]landReworkWeek, 0, len(weekNames))
	for _, week := range weekNames {
		row := landReworkWeek{Week: week}
		if t := byWeek[week]; t != nil {
			row.Beads, row.TwoRounds, row.TwoSeats = t.beads, t.twoRounds, t.twoSeats
			row.Timed = t.timed
			row.Untimed = t.beads - t.timed
			if t.beads >= landReworkStatsMinBeads {
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

// landReworkStatsWeekNames lists the weeks from first, oldest first.
func landReworkStatsWeekNames(first string, weeks int) ([]string, error) {
	start, err := time.ParseInLocation(landReworkStatsWeekLayout, first, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("the window's first week %q: %w", first, err)
	}
	names := make([]string, 0, weeks)
	for i := 0; i < weeks; i++ {
		names = append(names, start.AddDate(0, 0, 7*i).Format(landReworkStatsWeekLayout))
	}
	return names, nil
}

// writeLandReworkStatsJSON prints the report as the struct the table renders,
// so a reader can take every figure without scraping the columns.
func writeLandReworkStatsJSON(w io.Writer, report landReworkReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

// writeLandReworkStatsTable prints one row per week, oldest first. The table is
// built in a strings.Builder, which cannot fail, and written to w once, so a
// refused write to w is the run's error.
func writeLandReworkStatsTable(w io.Writer, report landReworkReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "rework stats: %d week(s), %s to %s\n", len(report.Weeks), report.FirstWeek, report.LastWeek)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Week\tBeads\t2+ rounds\t2+ seats\tMean polecat min\tNo time")
	for _, row := range report.Weeks {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%d\n", row.Week, row.Beads,
			landReworkStatsShareCell(row.TwoRoundsShare), landReworkStatsShareCell(row.TwoSeatsShare),
			landReworkStatsMeanCell(row), row.Untimed)
	}
	_ = tw.Flush()

	var notes []string
	if report.Outside > 0 {
		notes = append(notes, fmt.Sprintf("%d landing(s) outside the window", report.Outside))
	}
	if report.NoLandingTime > 0 {
		notes = append(notes, fmt.Sprintf("%d landing(s) with no landed_at, folded into no week", report.NoLandingTime))
	}
	if report.Skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d unreadable record line(s) skipped", report.Skipped))
	}
	if len(notes) > 0 {
		fmt.Fprintf(&b, "\n%s\n", strings.Join(notes, "\n"))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// landReworkStatsShareCell renders a share, or the reason the week has none.
func landReworkStatsShareCell(share *float64) string {
	if share == nil {
		return "too few to say"
	}
	return fmt.Sprintf("%.1f%%", *share*100)
}

// landReworkStatsMeanCell renders a week's mean polecat minutes, or the reason
// it has none.
func landReworkStatsMeanCell(row landReworkWeek) string {
	switch {
	case row.TooFew:
		return "too few to say"
	case row.MeanPolecatMinutes == nil:
		return "no timed rows"
	}
	return fmt.Sprintf("%.1f", *row.MeanPolecatMinutes)
}
