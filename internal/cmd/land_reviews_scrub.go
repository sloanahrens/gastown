package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/workspace"
)

var landReviewsScrubRecords string

// landReviewsScrubCmd runs the review log's scrub as a dry run: it reports the
// rows it would rewrite, by bead id, and writes no file.
var landReviewsScrubCmd = &cobra.Command{
	Use:   "reviews-scrub",
	Short: "Report the review log's one-time scrub without writing it",
	Long: `Report what scrubbing the review log would change.

Reads the town's review records (.runtime/landings/reviews.jsonl) and prints,
without writing, the row and word counts, the lines too long to examine, and
the changed rows' bead ids. No row text is printed.

Examples:
  gt land reviews-scrub
  gt land reviews-scrub --records /tmp/reviews.jsonl`,
	Args: cobra.NoArgs,
	RunE: runLandReviewsScrub,
}

func init() {
	landReviewsScrubCmd.Flags().StringVar(&landReviewsScrubRecords, "records", "", "review records file (default: <town>/.runtime/landings/reviews.jsonl)")
	landCmd.AddCommand(landReviewsScrubCmd)
}

func runLandReviewsScrub(cmd *cobra.Command, _ []string) error {
	return landReviewsScrubReportFrom(workspace.FindFromCwdOrError, cmd.OutOrStdout(), landReviewsScrubRecords)
}

// landReviewsScrubReportFrom is the command body over explicitly named inputs,
// so a test supplies the town without swapping the shared flag variables.
func landReviewsScrubReportFrom(findTown func() (string, error), out io.Writer, recordsFlag string) error {
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
	_, err = fmt.Fprintf(out, "reviews-scrub (dry run): %d rows, %d would change, %d words, oversize %d, skipped %d\nbeads: %s\n",
		res.Rows, res.RowsChanged, res.Words, res.Oversize, res.Skipped, strings.Join(res.Beads, ", "))
	if err != nil {
		return fmt.Errorf("writing the plan: %w", err)
	}
	return nil
}
