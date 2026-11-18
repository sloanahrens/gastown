package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/reworkstats"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	landSizeStatsWeeks int
	landSizeStatsJSON  bool
)

// landSizeStatsDefaultWeeks is the window a run reports without --weeks.
const landSizeStatsDefaultWeeks = 4

var landSizeStatsCmd = &cobra.Command{
	Use:   "size-stats",
	Short: "Report landings by added lines, with their time and attempt shares",
	Long: `Report the landed work by the size of the change it landed.

Each landing is bucketed by the lines its change added, and each bucket prints
the beads in it, the median polecat minutes they took, the share that needed
two or more polecat attempts and the share that took more than thirty minutes.

A record that carries no added lines, no polecat time or a time flagged as a
bound takes no bucket, because a bucket that folded any of them in would read
a short sample as a complete one. The count left out is printed under the
table, along with the landings outside the window and the record lines that
would not read, so a reader can tell a thin table from a complete one.

Reads the rig landing ledgers the other gt land commands read, keeping the
landings in the last --weeks weeks ending with the current one, and writes no
file. --json prints the same numbers.

Examples:
  gt land size-stats
  gt land size-stats --weeks 8
  gt land size-stats --json`,
	Args: cobra.NoArgs,
	RunE: runLandSizeStats,
}

func init() {
	landSizeStatsCmd.Flags().IntVar(&landSizeStatsWeeks, "weeks", landSizeStatsDefaultWeeks, "weeks of landings to report, ending with the current one")
	landSizeStatsCmd.Flags().BoolVar(&landSizeStatsJSON, "json", false, "print the report as JSON")
	landCmd.AddCommand(landSizeStatsCmd)
}

func runLandSizeStats(cmd *cobra.Command, _ []string) error {
	return landSizeStatsReportFrom(workspace.FindFromCwdOrError, cmd.OutOrStdout(), time.Now(), landSizeStatsWeeks, landSizeStatsJSON)
}

// landSizeStatsReport is the document both forms print: the buckets in report
// order, the count no bucket could take, and the counts the window left out,
// so a reader knows what the rows do not cover.
type landSizeStatsReport struct {
	FirstWeek string                `json:"first_week"`
	LastWeek  string                `json:"last_week"`
	Buckets   []landSizeStatsBucket `json:"buckets"`
	// Skipped counts the records no bucket could take: no added lines, no
	// polecat time, or a time flagged as a bound.
	Skipped int `json:"skipped"`
	// Outside counts the landings whose week is outside the window.
	Outside int `json:"outside_window"`
	// NoLandingTime counts the records with no landed_at: a week is read from
	// that field, so such a record folds into no week.
	NoLandingTime int `json:"no_landing_time"`
	// Unreadable counts the ledger lines that are not records.
	Unreadable int `json:"unreadable_lines"`
}

// landSizeStatsBucket is one added-lines bucket as both forms print it. A
// bucket with no record carries no figure, as a nil does in reworkstats.Week,
// so an empty bucket never reads as a fast one.
type landSizeStatsBucket struct {
	Label             string   `json:"label"`
	Beads             int      `json:"beads"`
	MedianMinutes     *float64 `json:"median_minutes"`
	MultiAttemptShare *float64 `json:"multi_attempt_share"`
	Over30Share       *float64 `json:"over_30_share"`
}

// landSizeStatsReportFrom is the command body over explicitly named inputs, so
// a test supplies the town and the clock without swapping the shared flag
// variables.
func landSizeStatsReportFrom(findTown func() (string, error), out io.Writer, now time.Time, weeks int, asJSON bool) error {
	report, err := landSizeStatsFromTown(findTown, now, weeks, metrics.WeekOf)
	if err != nil {
		return err
	}
	if asJSON {
		return writeLandSizeStatsJSON(out, report)
	}
	return writeLandSizeStatsTable(out, report)
}

// landSizeStatsFromTown reads the town's landing ledgers and buckets the
// records landed in the window. weekOf names a timestamp's week; the command
// passes metrics.WeekOf, and a test passes one that fails, to drive the refusal.
func landSizeStatsFromTown(findTown func() (string, error), now time.Time, weeks int, weekOf func(string) (string, error)) (landSizeStatsReport, error) {
	// The flag is refused before the town is resolved and its ledgers read, so
	// a bad --weeks fails on the flag rather than on a town.
	if weeks < 1 {
		return landSizeStatsReport{}, fmt.Errorf("--weeks %d: want at least 1", weeks)
	}
	town, err := findTown()
	if err != nil {
		return landSizeStatsReport{}, fmt.Errorf("resolving the town: %w", err)
	}
	recs, unreadable, err := reworkstats.ReadLandings(landReworkStatsLandingsDir(town))
	if err != nil {
		return landSizeStatsReport{}, err
	}
	firstWeek, lastWeek, err := landSizeStatsWindow(now, weeks, weekOf)
	if err != nil {
		return landSizeStatsReport{}, err
	}
	kept, outside, noTime, err := landSizeStatsInWindow(recs, firstWeek, lastWeek, weekOf)
	if err != nil {
		return landSizeStatsReport{}, err
	}
	buckets, skipped := reworkstats.SizeStats(kept)
	return landSizeStatsReport{
		FirstWeek:     firstWeek,
		LastWeek:      lastWeek,
		Buckets:       landSizeStatsBuckets(buckets),
		Skipped:       skipped,
		Outside:       outside,
		NoLandingTime: noTime,
		Unreadable:    unreadable,
	}, nil
}

// landSizeStatsWindow is the weeks the report covers: the week now falls in and
// the weeks-1 before it, oldest first.
func landSizeStatsWindow(now time.Time, weeks int, weekOf func(string) (string, error)) (string, string, error) {
	last, err := weekOf(now.Format(time.RFC3339Nano))
	if err != nil {
		return "", "", fmt.Errorf("the week of %s: %w", now.Format(time.RFC3339), err)
	}
	// Subtracting whole weeks keeps the weekday, so its week is the one weeks-1
	// before the last.
	first, err := weekOf(now.AddDate(0, 0, -7*(weeks-1)).Format(time.RFC3339Nano))
	if err != nil {
		return "", "", fmt.Errorf("the week of the window's first day: %w", err)
	}
	return first, last, nil
}

// landSizeStatsInWindow keeps the records landed in weeks first to last, both
// included, and counts the ones it leaves out: a landing outside the window,
// and a record with no landed_at, which is no week's record and is counted
// rather than read as absent.
func landSizeStatsInWindow(recs []landings.Record, first, last string, weekOf func(string) (string, error)) ([]landings.Record, int, int, error) {
	var kept []landings.Record
	outside, noLandingTime := 0, 0
	for _, rec := range recs {
		if rec.LandedAt.IsZero() {
			noLandingTime++
			continue
		}
		week, err := weekOf(rec.LandedAt.Format(time.RFC3339Nano))
		if err != nil {
			return nil, 0, 0, fmt.Errorf("the week of %s's landing: %w", rec.Bead, err)
		}
		// The names carry one format, so they sort as the days they name.
		if week < first || week > last {
			outside++
			continue
		}
		kept = append(kept, rec)
	}
	return kept, outside, noLandingTime, nil
}

// landSizeStatsBuckets renders SizeStats's buckets: one with a record carries
// its figures, an empty one carries none.
func landSizeStatsBuckets(from []reworkstats.SizeBucket) []landSizeStatsBucket {
	buckets := make([]landSizeStatsBucket, 0, len(from))
	for _, b := range from {
		row := landSizeStatsBucket{Label: b.Label, Beads: b.Beads}
		if b.Beads > 0 {
			// SizeStats gives every figure of a bucket that holds a record.
			median, multi, over30 := b.MedianMinutes, b.MultiAttemptShare, b.Over30Share
			row.MedianMinutes, row.MultiAttemptShare, row.Over30Share = &median, &multi, &over30
		}
		buckets = append(buckets, row)
	}
	return buckets
}

// writeLandSizeStatsJSON prints the report as one JSON document, the numbers
// the table renders.
func writeLandSizeStatsJSON(w io.Writer, report landSizeStatsReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

// writeLandSizeStatsTable prints one row per bucket, in report order, then the
// counts the rows leave out. The table is built in a strings.Builder, which
// cannot fail, and written to w once, so a refused write to w is the run's
// error.
func writeLandSizeStatsTable(w io.Writer, report landSizeStatsReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "size stats: %s to %s\n", report.FirstWeek, report.LastWeek)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Bucket\tBeads\tMedian min\t2+ attempts\tOver 30 min")
	for _, row := range report.Buckets {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", row.Label, row.Beads,
			landSizeStatsMinutesCell(row.MedianMinutes),
			landSizeStatsShareCell(row.MultiAttemptShare),
			landSizeStatsShareCell(row.Over30Share))
	}
	_ = tw.Flush()

	var notes []string
	if report.Skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d record(s) left out of every bucket: no added lines or no polecat time", report.Skipped))
	}
	if report.Outside > 0 {
		notes = append(notes, fmt.Sprintf("%d landing(s) outside the window", report.Outside))
	}
	if report.NoLandingTime > 0 {
		notes = append(notes, fmt.Sprintf("%d record(s) with no landed_at, in no week", report.NoLandingTime))
	}
	if report.Unreadable > 0 {
		notes = append(notes, fmt.Sprintf("%d unreadable record line(s) skipped", report.Unreadable))
	}
	if len(notes) > 0 {
		fmt.Fprintf(&b, "\n%s\n", strings.Join(notes, "\n"))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// landSizeStatsMinutesCell renders a bucket's median minutes, or "-" for a
// bucket no record reached.
func landSizeStatsMinutesCell(minutes *float64) string {
	if minutes == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *minutes)
}

// landSizeStatsShareCell renders a share, or "-" for a bucket no record
// reached.
func landSizeStatsShareCell(share *float64) string {
	if share == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *share*100)
}
