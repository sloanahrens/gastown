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

	"github.com/steveyegge/gastown/internal/daemonlog"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	landOmStatsSince   string
	landOmStatsRecords string
	landOmStatsLog     string
	landOmStatsJSON    bool
)

// landOmStatsCmd is the operator's quick view of om's verdicts. Merlin's mango
// pipeline reads the same records for its CSV; this writes nothing.
var landOmStatsCmd = &cobra.Command{
	Use:   "om-stats",
	Short: "Report how often om rejects landings, by day, rig and agent",
	Long: `Report how often om rejects landings.

Reads the town's review records (.runtime/landings/reviews.jsonl), one line
per om verdict, and aggregates them: per day the approvals, rejections and
rejection rate, the mean findings and score of the rejections; per rig and per
author seat the same counts; how many review rounds each bead's records
reached (its highest attempt); and the ten beads rejected most.

Days before the first review record are filled from the landing worker's
daemon log and marked with a * in the table, or fallback in the JSON; the log
names no rig and no seat, so those days count in the day table alone. Rounds
per bead and the beads rejected most come from the review records, because a
log line names neither an attempt nor a rig.

A path given with --records or --log must exist, so a typo is an error. The
town's two defaults may be absent — a town that has not landed yet has
neither — and that is an empty report, not a failure.

The command reads only: it writes no file. --json prints the same data as the
table.

Examples:
  gt land om-stats
  gt land om-stats --since 2026-10-20
  gt land om-stats --json --records /tmp/reviews.jsonl`,
	Args: cobra.NoArgs,
	RunE: runLandOmStats,
}

func init() {
	landOmStatsCmd.Flags().StringVar(&landOmStatsSince, "since", "", "first day to report, as YYYY-MM-DD (default: every day on record)")
	landOmStatsCmd.Flags().StringVar(&landOmStatsRecords, "records", "", "review records file (default: <town>/.runtime/landings/reviews.jsonl)")
	landOmStatsCmd.Flags().StringVar(&landOmStatsLog, "log", "", "daemon log for the days before the first record (default: <town>/daemon/daemon.log)")
	landOmStatsCmd.Flags().BoolVar(&landOmStatsJSON, "json", false, "print the report as JSON")
	landCmd.AddCommand(landOmStatsCmd)
}

func runLandOmStats(cmd *cobra.Command, _ []string) error {
	return landOmStatsReport(cmd.OutOrStdout(), landOmStatsSince, landOmStatsRecords, landOmStatsLog, landOmStatsJSON)
}

// landOmStatsFS is the read-only filesystem the report reads its two sources
// through, so a test supplies an in-memory one and the command has no way to
// write.
type landOmStatsFS interface {
	Open(name string) (io.ReadCloser, error)
}

type landOmStatsDisk struct{}

func (landOmStatsDisk) Open(name string) (io.ReadCloser, error) { return os.Open(name) }

// landOmStatsReport is the command body over explicitly named inputs, so a
// test drives every path without swapping the shared flag variables.
func landOmStatsReport(out io.Writer, sinceValue, recordsFlag, logFlag string, asJSON bool) error {
	return landOmStatsReportFrom(landOmStatsDisk{}, workspace.FindFromCwdOrError, out, sinceValue, recordsFlag, logFlag, asJSON)
}

func landOmStatsReportFrom(fsys landOmStatsFS, findTown func() (string, error), out io.Writer, sinceValue, recordsFlag, logFlag string, asJSON bool) error {
	since, err := parseLandOmStatsSince(sinceValue)
	if err != nil {
		return err
	}
	inputs, err := landOmStatsResolvePaths(recordsFlag, logFlag, findTown)
	if err != nil {
		return err
	}
	stats, err := landOmStatsFromFiles(fsys, inputs, since)
	if err != nil {
		return err
	}
	if asJSON {
		return writeLandOmStatsJSON(out, stats)
	}
	writeLandOmStatsTable(out, stats, since)
	return nil
}

// landOmStatsInputs is the two files a report reads, and whether the operator
// named each one rather than leaving it to the town default.
type landOmStatsInputs struct {
	records, log           string
	recordsNamed, logNamed bool
}

// landOmStatsResolvePaths fills each empty flag with the town's file. A named
// path must exist, so a typo cannot read as a clean empty report; a default may
// be missing, because a town that has landed nothing yet has neither file.
func landOmStatsResolvePaths(recordsFlag, logFlag string, findTown func() (string, error)) (landOmStatsInputs, error) {
	in := landOmStatsInputs{
		records: strings.TrimSpace(recordsFlag),
		log:     strings.TrimSpace(logFlag),
	}
	in.recordsNamed, in.logNamed = in.records != "", in.log != ""
	if in.recordsNamed && in.logNamed {
		return in, nil
	}
	townRoot, err := findTown()
	if err != nil {
		return landOmStatsInputs{}, fmt.Errorf("resolving the town: %w", err)
	}
	if !in.recordsNamed {
		in.records = land.TownReviewsFile(townRoot).Path
	}
	if !in.logNamed {
		in.log = filepath.Join(townRoot, "daemon", daemonlog.FileName)
	}
	return in, nil
}

// parseLandOmStatsSince reads --since as a calendar day. An empty value is the
// zero time, which filters nothing.
func parseLandOmStatsSince(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	at, err := time.ParseInLocation(land.OmStatsDayLayout, value, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: want a day as YYYY-MM-DD", value)
	}
	return at, nil
}

// landOmStatsFromFiles opens both sources and aggregates them.
func landOmStatsFromFiles(fsys landOmStatsFS, in landOmStatsInputs, since time.Time) (land.OmStats, error) {
	records, closeRecords, err := openLandOmStatsSource(fsys, in.records, in.recordsNamed)
	if err != nil {
		return land.OmStats{}, err
	}
	defer closeRecords()
	logFile, closeLog, err := openLandOmStatsSource(fsys, in.log, in.logNamed)
	if err != nil {
		return land.OmStats{}, err
	}
	defer closeLog()
	return land.AggregateOmStats(records, logFile, since)
}

// openLandOmStatsSource opens path for reading; a missing named path is an
// error, a missing default is a nil reader with nothing to close.
func openLandOmStatsSource(fsys landOmStatsFS, path string, named bool) (io.Reader, func(), error) {
	f, err := fsys.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		if named {
			return nil, nil, fmt.Errorf("%s does not exist: %w", path, err)
		}
		return nil, func() {}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return f, func() { _ = f.Close() }, nil
}

// writeLandOmStatsJSON prints the report as the struct the table renders, so a
// reader can take every figure without scraping the columns.
func writeLandOmStatsJSON(w io.Writer, stats land.OmStats) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(stats); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

// writeLandOmStatsTable prints the report one question at a time: how a day,
// a rig and a seat land, and which beads take the most rounds.
func writeLandOmStatsTable(w io.Writer, stats land.OmStats, since time.Time) {
	header := "om landings, all days"
	if !since.IsZero() {
		header = "om landings since " + since.Format(land.OmStatsDayLayout)
	}
	fmt.Fprintln(w, header)
	if len(stats.Days) == 0 {
		fmt.Fprintln(w, "no om verdicts in range")
	}
	if len(stats.Days) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "Day\tApprovals\tRejections\tRate\tMean findings\tMean score")
		fallback := false
		for _, d := range stats.Days {
			mark := ""
			if d.Fallback {
				mark, fallback = "*", true
			}
			fmt.Fprintf(tw, "%s%s\t%d\t%d\t%s\t%.1f\t%.2f\n", d.Day, mark, d.Approvals, d.Rejections,
				landOmStatsPercent(d.Rate), d.MeanFindings, d.MeanScore)
		}
		_ = tw.Flush()
		if fallback {
			fmt.Fprintln(w, "\n* filled from the daemon log: no review record for the day")
		}
	}
	if len(stats.Rigs) > 0 {
		fmt.Fprintln(w)
		writeLandOmStatsGroups(w, "Rig", stats.Rigs)
	}
	if len(stats.Agents) > 0 {
		fmt.Fprintln(w)
		writeLandOmStatsGroups(w, "Agent", stats.Agents)
	}
	fmt.Fprintf(w, "\nRounds per bead: %d took one, %d two, %d three or more\n",
		stats.Rounds.One, stats.Rounds.Two, stats.Rounds.ThreePlus)
	if len(stats.TopBeads) > 0 {
		fmt.Fprintln(w, "\nBeads rejected most")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "Bead\tRig\tRejections")
		for _, b := range stats.TopBeads {
			fmt.Fprintf(tw, "%s\t%s\t%d\n", b.Bead, landOmStatsRig(b.Rig), b.Rejections)
		}
		_ = tw.Flush()
	}
	if stats.Skipped > 0 {
		fmt.Fprintf(w, "\n%d unparseable review-record line(s) skipped\n", stats.Skipped)
	}
}

func writeLandOmStatsGroups(w io.Writer, title string, groups []land.OmStatsGroup) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tApprovals\tRejections\tRate\n", title)
	for _, g := range groups {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", g.Name, g.Approvals, g.Rejections, landOmStatsPercent(g.Rate))
	}
	_ = tw.Flush()
}

func landOmStatsPercent(rate float64) string {
	return fmt.Sprintf("%.1f%%", rate*100)
}

func landOmStatsRig(rig string) string {
	if strings.TrimSpace(rig) == "" {
		return land.OmStatsUnnamed
	}
	return rig
}
