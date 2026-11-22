package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	landReviewsScrubRecords string
	landReviewsScrubJSON    bool
	landReviewsScrubApply   bool
)

// landReviewsScrubCmd runs the review log's scrub. By default it is a dry run:
// it reports the rows it would rewrite, by bead id, and writes no file. With
// --apply it rewrites the log.
var landReviewsScrubCmd = &cobra.Command{
	Use:   "reviews-scrub",
	Short: "Report or apply the review log's one-time scrub",
	Long: `Scrub the review log's token-shaped words.

Reads the town's review records (.runtime/landings/reviews.jsonl) and prints,
without writing, the row and word counts, the lines too long to examine, and
the changed rows' bead ids. No row text is printed. --json prints the same
figures as an object. --apply rewrites the log, replacing each flagged word; a
second run finds nothing to change. A row it skips is kept and named under
skipped beads; --apply exits 1 when any row is skipped or oversize. A missing
log is an error naming its path.

Examples:
  gt land reviews-scrub
  gt land reviews-scrub --records /tmp/reviews.jsonl
  gt land reviews-scrub --json
  gt land reviews-scrub --apply`,
	Args: cobra.NoArgs,
	RunE: runLandReviewsScrub,
}

func init() {
	landReviewsScrubCmd.Flags().StringVar(&landReviewsScrubRecords, "records", "", "review records file (default: <town>/.runtime/landings/reviews.jsonl)")
	landReviewsScrubCmd.Flags().BoolVar(&landReviewsScrubJSON, "json", false, "print the plan as JSON")
	landReviewsScrubCmd.Flags().BoolVar(&landReviewsScrubApply, "apply", false, "rewrite the review log, replacing each flagged word")
	landCmd.AddCommand(landReviewsScrubCmd)
}

func runLandReviewsScrub(cmd *cobra.Command, _ []string) error {
	return landReviewsScrubReportFrom(workspace.FindFromCwdOrError, cmd.OutOrStdout(), landReviewsScrubRecords, landReviewsScrubApply, landReviewsScrubJSON)
}

// landReviewsScrubReportFrom is the command body over explicitly named inputs,
// so a test supplies the town without swapping the shared flag variables.
func landReviewsScrubReportFrom(findTown func() (string, error), out io.Writer, recordsFlag string, apply, asJSON bool) error {
	logf := &land.ReviewsFile{Path: strings.TrimSpace(recordsFlag)}
	if logf.Path == "" {
		town, err := findTown()
		if err != nil {
			return fmt.Errorf("resolving the town: %w", err)
		}
		logf = land.TownReviewsFile(town)
	}
	// The scrub reads a missing log as nothing to scrub; the command refuses it,
	// so a mistyped path is not reported as a clean log.
	if _, err := os.Stat(logf.Path); err != nil {
		return fmt.Errorf("reading the review log: %w", err)
	}
	var res land.ScrubResult
	var err error
	if apply {
		res, err = logf.Scrub(true)
	} else {
		res, err = logf.ScrubPlan()
	}
	if err != nil {
		return err
	}
	return writeLandReviewsScrubReport(out, res, apply, asJSON)
}

// writeLandReviewsScrubReport prints one scrub's figures, text or JSON, and fails an
// --apply that left rows it could not rewrite. It takes the result whole, so a test
// drives it with a ScrubResult no real log can produce cheaply (an oversize row is a
// 16 MiB line).
func writeLandReviewsScrubReport(out io.Writer, res land.ScrubResult, apply, asJSON bool) error {
	if asJSON {
		if err := writeLandReviewsScrubJSON(out, res, apply); err != nil {
			return err
		}
		return landReviewsScrubApplyExit(res, apply)
	}
	// A dry run reports rows that would change; an applied run already rewrote them.
	mode, changed := "dry run", "would change"
	if apply {
		mode, changed = "applied", "changed"
	}
	_, err := fmt.Fprintf(out, "reviews-scrub (%s): %d rows, %d %s, %d words, oversize %d, skipped %d\nbeads: %s\n",
		mode, res.Rows, res.RowsChanged, changed, res.Words, res.Oversize, res.Skipped, strings.Join(res.Beads, ", "))
	if err != nil {
		return fmt.Errorf("writing the plan: %w", err)
	}
	if res.Skipped != 0 || res.Oversize != 0 {
		if _, err := fmt.Fprintf(out, "skipped beads: %s\n", landReviewsScrubSkippedLine(res)); err != nil {
			return fmt.Errorf("writing the plan: %w", err)
		}
	}
	return landReviewsScrubApplyExit(res, apply)
}

// landReviewsScrubApplyExit fails an --apply that left rows the scrub could not
// rewrite: a skipped row is kept byte for byte, and an oversize line is not
// decoded, so the run must not exit clean. A dry run only reports them.
func landReviewsScrubApplyExit(res land.ScrubResult, apply bool) error {
	if !apply || (res.Skipped == 0 && res.Oversize == 0) {
		return nil
	}
	return fmt.Errorf("reviews-scrub --apply left %d skipped and %d oversize row(s) in the log", res.Skipped, res.Oversize)
}

// landReviewsScrubSkippedLine is what follows `skipped beads:`: the skipped rows by
// label, then the oversize count. An oversize line is never decoded, so it has no
// bead id to list; a run with only oversize rows reads `none; N oversize rows`, so
// an --apply that exits 1 names its cause.
func landReviewsScrubSkippedLine(res land.ScrubResult) string {
	ids, oversize := res.SkippedBeads, res.Oversize
	line := "none"
	if len(ids) > 0 {
		line = strings.Join(ids, ", ")
	}
	switch {
	case oversize == 1:
		line += "; 1 oversize row"
	case oversize > 1:
		line += fmt.Sprintf("; %d oversize rows", oversize)
	}
	return line
}

// landReviewsScrubPlan is the scrub's figures as --json prints them.
type landReviewsScrubPlan struct {
	DryRun      bool     `json:"dry_run"`
	Rows        int      `json:"rows"`
	WouldChange int      `json:"would_change"`
	Words       int      `json:"words"`
	Oversize    int      `json:"oversize"`
	Skipped     int      `json:"skipped"`
	Beads       []string `json:"beads"`
}

func writeLandReviewsScrubJSON(w io.Writer, res land.ScrubResult, apply bool) error {
	beads := res.Beads
	if beads == nil {
		beads = []string{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	err := enc.Encode(landReviewsScrubPlan{
		DryRun: !apply, Rows: res.Rows, WouldChange: res.RowsChanged, Words: res.Words,
		Oversize: res.Oversize, Skipped: res.Skipped, Beads: beads,
	})
	if err != nil {
		return fmt.Errorf("writing the plan: %w", err)
	}
	return nil
}
