package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/reworkstats"
	"github.com/steveyegge/gastown/internal/workspace"
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

The two figures come from the landing record: the share of landed beads that
needed two or more om review rounds, and the mean polecat minutes per landed
bead. The report prints both per week, beside the share that needed two or more
polecat seats and the week's bead count. docs/design/rework-program.md (lever 6)
says what they are for.

Reads the rig landing ledgers (.runtime/landings/<rig>.jsonl) and the review log
(.runtime/landings/reviews.jsonl), the files the other gt land commands read. A
bead's rounds are the highest attempt its review records carry; its seats are
the polecat_attempts its landing record carries. A landing recorded with no
polecat time is counted in the No time column and left out of the mean, never
read as zero. A bead landed twice counts once, in the week of its last landing.

A week too thin to give a share or a mean prints "too few to say" in their place.

The command reads only: it writes no file. --json prints the same numbers.

Examples:
  gt land rework-stats
  gt land rework-stats --weeks 12
  gt land rework-stats --json`,
	Args: cobra.NoArgs,
	RunE: runLandReworkStats,
}

func init() {
	landReworkStatsCmd.Flags().IntVar(&landReworkStatsWeeks, "weeks", reworkstats.DefaultWeeks, "weeks to report, ending with the current one")
	landReworkStatsCmd.Flags().BoolVar(&landReworkStatsJSON, "json", false, "print the report as JSON")
	landCmd.AddCommand(landReworkStatsCmd)
}

func runLandReworkStats(cmd *cobra.Command, _ []string) error {
	return landReworkStatsReportFrom(workspace.FindFromCwdOrError, cmd.OutOrStdout(), time.Now(), landReworkStatsWeeks, landReworkStatsJSON)
}

// landReworkReport names the package's report for the command's tests, which
// predate the move into reworkstats.
type landReworkReport = reworkstats.Report

// landReworkStatsReportFrom is the command body over explicitly named inputs,
// so a test supplies the town and the clock without swapping the shared flag
// variables.
func landReworkStatsReportFrom(findTown func() (string, error), out io.Writer, now time.Time, weeks int, asJSON bool) error {
	report, err := landReworkStatsFromTown(findTown, now, weeks, metrics.WeekOf)
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
func landReworkStatsFromTown(findTown func() (string, error), now time.Time, weeks int, weekOf func(string) (string, error)) (reworkstats.Report, error) {
	// The flag is refused before the town is resolved and its ledgers read, so
	// a bad --weeks fails on the flag rather than on a town.
	if weeks < 1 {
		return reworkstats.Report{}, fmt.Errorf("--weeks %d: want at least 1", weeks)
	}
	town, err := findTown()
	if err != nil {
		return reworkstats.Report{}, fmt.Errorf("resolving the town: %w", err)
	}
	dir := landReworkStatsLandingsDir(town)
	recs, skipped, err := reworkstats.ReadLandings(dir)
	if err != nil {
		return reworkstats.Report{}, err
	}
	reviews, unreadable, err := reworkstats.ReadReviews(land.TownReviewsFile(town).Path)
	if err != nil {
		return reworkstats.Report{}, err
	}
	report, err := landReworkStatsAggregate(recs, reviews, now, weeks, weekOf)
	if err != nil {
		return reworkstats.Report{}, err
	}
	report.Skipped += skipped + unreadable
	return report, nil
}

// landReworkStatsLandingsDir is where the landing worker keeps every rig's
// ledger and the town's review log.
func landReworkStatsLandingsDir(town string) string {
	return filepath.Join(constants.TownRuntimePath(town), "landings")
}

// landReworkStatsAggregate is reworkstats.Compute under the name the command's
// tests drive the week-refusal table through.
func landReworkStatsAggregate(recs []landings.Record, reviews []land.ReviewRecord, now time.Time, weeks int, weekOf func(string) (string, error)) (reworkstats.Report, error) {
	return reworkstats.Compute(recs, reviews, now, weeks, weekOf)
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
func landReworkStatsMeanCell(row reworkstats.Week) string {
	switch {
	case row.TooFew:
		return "too few to say"
	case row.MeanPolecatMinutes == nil:
		return "no timed rows"
	}
	return fmt.Sprintf("%.1f", *row.MeanPolecatMinutes)
}
