package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	jevScorecardBattery string
	jevScorecardSince   string
	jevScorecardJSON    bool
)

var jevScorecardCmd = &cobra.Command{
	Use:   "scorecard",
	Short: "Score a battery's answers against its labels",
	Long: `Read a battery's JSONL log through its registered adapter and print, for each
question, how Jev's answers agree with the labels the adapter finds: lines
logged and labeled, positives and negatives, precision and recall (at 0.5 for a
noul, at the chosen option for a choice), the cut with the best F1, five
calibration bands, and a verdict. A score question reports the mean score by
label and the best cut on the score in place of precision, recall and bands.

The verdict is earning when the best F1 beats the label base rate by 0.1 or
more on at least 30 labeled lines, not-yet below 30 labeled lines, and
no-signal otherwise. The command is read-only.

--battery is required. An unknown name is an error that lists the registered
batteries. --since keeps only the lines stamped in the window ending now, given
as Nd for days or a Go duration such as 36h. --json prints the same report as
JSON.

Examples:
  gt jev scorecard --battery fixture
  gt jev scorecard --battery fixture --since 14d --json`,
	Args: cobra.NoArgs,
	RunE: runJevScorecard,
}

func init() {
	f := jevScorecardCmd.Flags()
	f.StringVar(&jevScorecardBattery, "battery", "", "Battery to score (required)")
	f.StringVar(&jevScorecardSince, "since", "", "Score only lines from this window ending now: Nd or a Go duration (default: all time)")
	f.BoolVar(&jevScorecardJSON, "json", false, "Print the report as JSON")
	_ = jevScorecardCmd.MarkFlagRequired("battery")
	jevCmd.AddCommand(jevScorecardCmd)
}

func runJevScorecard(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return jevScorecardWithFlags(cmd.Context(), cmd.OutOrStdout(), townRoot, jevScorecardBattery, jevScorecardSince, jevScorecardJSON, time.Now())
}

// openScorecardAdapter builds a battery's adapter. A test swaps it to put fakes
// behind an adapter that reads the rig's beads.
var openScorecardAdapter = scorecard.Open

// jevScorecardWithFlags resolves the window and the battery, then prints the
// report for the town at townRoot.
func jevScorecardWithFlags(ctx context.Context, w io.Writer, townRoot, battery, since string, asJSON bool, now time.Time) error {
	from, err := parseSince(since, now)
	if err != nil {
		return err
	}
	a, err := openScorecardAdapter(battery, townRoot)
	if err != nil {
		return err
	}
	rep, err := scorecard.Build(ctx, a, from)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return scorecard.WriteText(w, rep)
}

// parseSince returns the start of the --since window ending at now. An empty
// value is all time, which is the zero time.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 1 {
			return time.Time{}, fmt.Errorf("--since %q: days must be a whole number of at least 1", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(s)
		if err != nil || parsed <= 0 {
			return time.Time{}, fmt.Errorf("--since %q: want Nd or a positive Go duration such as 36h", s)
		}
		d = parsed
	}
	return now.Add(-d), nil
}
