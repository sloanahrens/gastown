package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	landReviewsScrubRecords string
	landReviewsScrubJSON    bool
)

// landReviewsScrubCmd runs the review log's scrub as a dry run: it reports the
// rows it would rewrite, by bead id, and writes no file.
var landReviewsScrubCmd = &cobra.Command{
	Use:   "reviews-scrub",
	Short: "Report the review log's one-time scrub without writing it",
	Long: `Report what scrubbing the review log would change.

Reads the town's review records (.runtime/landings/reviews.jsonl) and prints,
without writing, the row and word counts, the lines too long to examine, and
the changed rows' bead ids. No row text is printed. --json prints the same
figures as an object.

Examples:
  gt land reviews-scrub
  gt land reviews-scrub --records /tmp/reviews.jsonl
  gt land reviews-scrub --json`,
	Args: cobra.NoArgs,
	RunE: runLandReviewsScrub,
}

func init() {
	landReviewsScrubCmd.Flags().StringVar(&landReviewsScrubRecords, "records", "", "review records file (default: <town>/.runtime/landings/reviews.jsonl)")
	landReviewsScrubCmd.Flags().BoolVar(&landReviewsScrubJSON, "json", false, "print the plan as JSON")
	landCmd.AddCommand(landReviewsScrubCmd)
}

func runLandReviewsScrub(cmd *cobra.Command, _ []string) error {
	return landReviewsScrubReportFrom(workspace.FindFromCwdOrError, cmd.OutOrStdout(), landReviewsScrubRecords, landReviewsScrubJSON)
}

// landReviewsScrubReportFrom is the command body over explicitly named inputs,
// so a test supplies the town without swapping the shared flag variables.
func landReviewsScrubReportFrom(findTown func() (string, error), out io.Writer, recordsFlag string, asJSON bool) error {
	logf := &land.ReviewsFile{Path: strings.TrimSpace(recordsFlag)}
	if logf.Path == "" {
		town, err := findTown()
		if err != nil {
			return fmt.Errorf("resolving the town: %w", err)
		}
		logf = land.TownReviewsFile(town)
	}
	res, err := logf.ScrubPlan()
	if err != nil {
		return err
	}
	if asJSON {
		return writeLandReviewsScrubJSON(out, res)
	}
	_, err = fmt.Fprintf(out, "reviews-scrub (dry run): %d rows, %d would change, %d words, oversize %d, skipped %d\nbeads: %s\n",
		res.Rows, res.RowsChanged, res.Words, res.Oversize, res.Skipped, strings.Join(res.Beads, ", "))
	if err != nil {
		return fmt.Errorf("writing the plan: %w", err)
	}
	return nil
}

// landReviewsScrubPlan is the dry run's figures as --json prints them.
type landReviewsScrubPlan struct {
	DryRun      bool     `json:"dry_run"`
	Rows        int      `json:"rows"`
	WouldChange int      `json:"would_change"`
	Words       int      `json:"words"`
	Oversize    int      `json:"oversize"`
	Skipped     int      `json:"skipped"`
	Beads       []string `json:"beads"`
}

func writeLandReviewsScrubJSON(w io.Writer, res land.ScrubResult) error {
	beads := res.Beads
	if beads == nil {
		beads = []string{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	err := enc.Encode(landReviewsScrubPlan{
		DryRun: true, Rows: res.Rows, WouldChange: res.RowsChanged, Words: res.Words,
		Oversize: res.Oversize, Skipped: res.Skipped, Beads: beads,
	})
	if err != nil {
		return fmt.Errorf("writing the plan: %w", err)
	}
	return nil
}
