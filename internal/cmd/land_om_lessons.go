package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	landOmLessonsSince   string
	landOmLessonsDays    int
	landOmLessonsRecords string
	landOmLessonsJSON    bool
)

// landOmLessonsCmd counts the rejection classes the weekly lessons pass works
// from. It reads the review log om-stats reads, and writes nothing.
var landOmLessonsCmd = &cobra.Command{
	Use:   "om-lessons",
	Short: "Cluster om's findings into rejection classes, by window",
	Long: `Cluster om's findings into the rejection classes the lessons pass counts.

Reads the town's review records (.runtime/landings/reviews.jsonl) and prints, per
class of the class table, the findings, the beads they were found on and the
landing rounds burned — the attempts past the first on a bead where the class
carried a major or critical finding — in the window and in the window before it,
so a class that grew or shrank reads at a glance.

A window is --days calendar days (7 by default). Without --since it ends today,
so each run counts the week just gone; --since pins its first day.

A finding that matches no keyword of the class table is unclassified, and the
report lists its title: that list is how a class the table is missing shows
itself.

A path given with --records must exist, so a typo is an error. The town's default
may be absent, and a town that has not landed yet is an empty report. A record
written before the log kept finding text carries none; those are counted apart
and fail nothing.

The command reads only: it writes no file.

Examples:
  gt land om-lessons
  gt land om-lessons --since 2026-10-01 --days 14
  gt land om-lessons --json --records scratch/reviews.jsonl`,
	Args: cobra.NoArgs,
	RunE: runLandOmLessons,
}

func init() {
	landOmLessonsCmd.Flags().StringVar(&landOmLessonsSince, "since", "", "first day of the window, as YYYY-MM-DD (default: a window ending today)")
	landOmLessonsCmd.Flags().IntVar(&landOmLessonsDays, "days", omlessons.DefaultDays, "window length in days; the prior window is the days before it")
	landOmLessonsCmd.Flags().StringVar(&landOmLessonsRecords, "records", "", "review records file (default: <town>/.runtime/landings/reviews.jsonl)")
	landOmLessonsCmd.Flags().BoolVar(&landOmLessonsJSON, "json", false, "print the report as JSON")
	landCmd.AddCommand(landOmLessonsCmd)
}

func runLandOmLessons(cmd *cobra.Command, _ []string) error {
	return landOmLessonsReport(cmd.OutOrStdout(), time.Now(), landOmLessonsSince, landOmLessonsDays, landOmLessonsRecords, landOmLessonsJSON)
}

// landOmLessonsReport is the command body with the clock named, so a test
// drives both windows without a flag.
func landOmLessonsReport(out io.Writer, now time.Time, sinceValue string, days int, recordsFlag string, asJSON bool) error {
	return landOmLessonsReportFrom(workspace.FindFromCwdOrError, out, now, sinceValue, days, recordsFlag, asJSON)
}

// landOmLessonsReportFrom is the command body over explicitly named inputs, so
// a test supplies the town, the clock and the records without swapping the
// shared flag variables.
func landOmLessonsReportFrom(findTown func() (string, error), out io.Writer, now time.Time, sinceValue string, days int, recordsFlag string, asJSON bool) error {
	since, err := parseLandOmStatsSince(sinceValue)
	if err != nil {
		return err
	}
	span, err := omlessons.Windows(now, since, days)
	if err != nil {
		return err
	}
	path, named, err := landOmLessonsRecordsPath(recordsFlag, findTown)
	if err != nil {
		return err
	}
	records, err := readLandOmLessonsRecords(path, named, span.Prior.Start)
	if err != nil {
		return err
	}
	report := omlessons.Cluster(records, span)
	if asJSON {
		return writeLandOmLessonsJSON(out, report)
	}
	return writeLandOmLessonsTable(out, report)
}

// landOmLessonsRecordsPath fills an empty --records with the town's review
// log, and reports whether the operator named the path.
func landOmLessonsRecordsPath(recordsFlag string, findTown func() (string, error)) (path string, named bool, err error) {
	if path = strings.TrimSpace(recordsFlag); path != "" {
		return path, true, nil
	}
	townRoot, err := findTown()
	if err != nil {
		return "", false, fmt.Errorf("resolving the town: %w", err)
	}
	return land.TownReviewsFile(townRoot).Path, false, nil
}

// readLandOmLessonsRecords reads the verdicts at or after since through the
// review log's own reader. The reader reads an absent log as no records,
// because a town that has not landed yet has none; a path the operator named
// must exist, so a typo cannot read as that same empty report.
func readLandOmLessonsRecords(path string, named bool, since time.Time) ([]land.ReviewRecord, error) {
	if named {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s does not exist: %w", path, err)
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	records, err := (&land.ReviewsFile{Path: path}).Since(since)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return records, nil
}

// writeLandOmLessonsJSON prints the report as the struct the table renders, so
// a reader can take every figure without scraping the columns.
func writeLandOmLessonsJSON(w io.Writer, report omlessons.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

// writeLandOmLessonsTable prints a row per class with the current window first
// and the prior one beside it, then the titles no class matched. The table is
// built in a strings.Builder, which cannot fail, and written to w once, so a
// refused write to w is the run's error.
func writeLandOmLessonsTable(w io.Writer, report omlessons.Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "om lessons: %s (%d days), against %s\n",
		landOmLessonsRange(report.Window), report.Days, landOmLessonsRange(report.Prior))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Class\tRule\tFindings\tBeads\tRounds\tPrior findings\tPrior beads\tPrior rounds")
	for _, c := range report.Classes {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\n", c.Name, landOmLessonsRule(c.Rule),
			c.Current.Findings, c.Current.Beads, c.Current.Rounds,
			c.Prior.Findings, c.Prior.Beads, c.Prior.Rounds)
	}
	_ = tw.Flush()

	if len(report.Unclassified) > 0 {
		fmt.Fprintln(&b, "\nUnclassified")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "Findings\tPrior\tTitle")
		for _, u := range report.Unclassified {
			title := "(no title)"
			if u.Title != "" {
				title = terminalSafe(u.Title)
			}
			fmt.Fprintf(tw, "%d\t%d\t%s\n", u.Current, u.Prior, title)
		}
		_ = tw.Flush()
	}

	fmt.Fprintf(&b, "\n%d verdict(s) in the window, %d in the prior; %d and %d carry no finding text\n",
		report.Window.Verdicts, report.Prior.Verdicts, report.Window.NoFindingText, report.Prior.NoFindingText)
	_, err := io.WriteString(w, b.String())
	return err
}

func landOmLessonsRange(window omlessons.Window) string {
	return window.First + " to " + window.Last
}

func landOmLessonsRule(rule int) string {
	if rule == 0 {
		return "-"
	}
	return fmt.Sprintf("#%d", rule)
}
