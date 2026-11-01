package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	metricsRepeatsSince string
	metricsRepeatsJSON  bool
)

// metricsRepeatsCmd is the operator's view of om asking for the same thing
// twice. The review log re-hashes a finding's id every round, so the count
// comes from the finding text, not the id (gt-657q2).
var metricsRepeatsCmd = &cobra.Command{
	Use:   "repeats",
	Short: "Report om findings that restate a finding of an earlier round",
	Long: `Report om findings that restate a finding an earlier round of the same bead
already raised.

Reads the town's review records (.runtime/landings/reviews.jsonl) and matches
each finding against the findings of the bead's earlier attempts (gt-657q2).
Two findings match when they name the same file and their title and claim share
at least metrics.RepeatSimilarityThreshold of their words, so a round that
rewords a request still counts.

A record whose finding count exceeds the finding text it records (a line
written before the log kept finding_text) names findings this report cannot
match, so they are in neither the findings nor the repeats, and the report says
how many records were such.

A round before --since is not a baseline: repeats are counted only between the
rounds the range holds.

The command reads only: it writes no file.

Examples:
  gt metrics repeats
  gt metrics repeats --since 2026-10-25
  gt metrics repeats --since 2026-10-25 --json`,
	Args: cobra.NoArgs,
	RunE: runMetricsRepeats,
}

func init() {
	metricsRepeatsCmd.Flags().StringVar(&metricsRepeatsSince, "since", "", "first day to report, as YYYY-MM-DD (default: every round on record)")
	metricsRepeatsCmd.Flags().BoolVar(&metricsRepeatsJSON, "json", false, "print the report as JSON")
	metricsCmd.AddCommand(metricsRepeatsCmd)
}

func runMetricsRepeats(cmd *cobra.Command, _ []string) error {
	return metricsRepeatsReport(cmd.OutOrStdout(), metricsRepeatsSince, metricsRepeatsJSON)
}

// metricsRepeatsFS is the read-only filesystem the report reads through, so a
// test supplies an in-memory one and the command has no way to write.
type metricsRepeatsFS interface {
	Open(name string) (io.ReadCloser, error)
}

type metricsRepeatsDisk struct{}

func (metricsRepeatsDisk) Open(name string) (io.ReadCloser, error) { return os.Open(name) }

// metricsRepeatsReport is the command body over explicitly named inputs, so a
// test drives every path without swapping the shared flag variables.
func metricsRepeatsReport(out io.Writer, sinceValue string, asJSON bool) error {
	return metricsRepeatsReportFrom(metricsRepeatsDisk{}, workspace.FindFromCwdOrError, out, sinceValue, asJSON)
}

func metricsRepeatsReportFrom(fsys metricsRepeatsFS, findTown func() (string, error), out io.Writer, sinceValue string, asJSON bool) error {
	since, err := parseLandOmStatsSince(sinceValue)
	if err != nil {
		return err
	}
	townRoot, err := findTown()
	if err != nil {
		return fmt.Errorf("resolving the town: %w", err)
	}
	report, err := metricsRepeatsRead(fsys, land.TownReviewsFile(townRoot).Path, since)
	if err != nil {
		return err
	}
	if asJSON {
		return writeMetricsRepeatsJSON(out, report)
	}
	writeMetricsRepeatsTable(out, report, since)
	return nil
}

// metricsRepeatsRead opens the town's review log and aggregates it. A town that
// has landed nothing has no log yet, which is an empty report rather than an
// error; a log that exists but cannot be read fails the report.
func metricsRepeatsRead(fsys metricsRepeatsFS, path string, since time.Time) (metrics.RepeatReport, error) {
	f, err := fsys.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		report := metrics.RepeatedFindings(nil)
		report.NoLogAt = path
		return report, nil
	}
	if err != nil {
		return metrics.RepeatReport{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	report, err := metrics.ReadRepeatReport(f, since)
	if err != nil {
		return metrics.RepeatReport{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return report, nil
}

// writeMetricsRepeatsJSON prints the report as the struct the table renders, so
// a reader can take every figure without scraping the columns.
func writeMetricsRepeatsJSON(w io.Writer, report metrics.RepeatReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

// writeMetricsRepeatsTable prints the overall share, the beads that repeat
// most, and what the share does not cover.
func writeMetricsRepeatsTable(w io.Writer, report metrics.RepeatReport, since time.Time) {
	header := "Repeated om findings, all rounds"
	if !since.IsZero() {
		header = "Repeated om findings since " + since.Format(land.OmStatsDayLayout)
	}
	fmt.Fprintln(w, header)
	fmt.Fprintf(w, "Findings %d, repeated %d (%s)\n", report.Findings, report.Repeated, metricsRepeatsPercent(report.RepeatShare))
	if report.NoLogAt != "" {
		fmt.Fprintf(w, "\nno review log at %s: nothing has landed in this town yet, or this is not the town you meant\n", report.NoLogAt)
	}
	if len(report.Beads) > 0 {
		fmt.Fprintln(w, "\nBeads repeating most")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "Bead\tRig\tFindings\tRepeated\tShare")
		for _, b := range report.Beads {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", b.Bead, landOmStatsRig(b.Rig), b.Findings, b.Repeated, metricsRepeatsPercent(b.Share))
		}
		_ = tw.Flush()
	}
	if report.NoText > 0 {
		fmt.Fprintf(w, "\n%d record(s) named a finding count and no finding text; those findings are not counted\n", report.NoText)
	}
	if report.Skipped > 0 {
		fmt.Fprintf(w, "\n%d unparseable review-record line(s) skipped\n", report.Skipped)
	}
}

func metricsRepeatsPercent(share float64) string {
	return fmt.Sprintf("%.1f%%", share*100)
}
