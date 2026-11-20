package metrics

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// The story-points evaluation step (gt-htz2cb): the held-out error of the
// estimate over the town's landed beads, printed over the test part the split
// names. The join is gt-mo0p0v, the split and the statistics gt-wizcsy.

// storyPointsEvalMinTest is the fewest test rows the error figures are printed
// from. Under it the part is too small to read, so the step prints the join's
// counts alone rather than a figure an operator would trust.
const storyPointsEvalMinTest = 30

// storyPointsEvalMinGroup is the fewest test rows a split group prints its
// figures from. A group is one review-round bucket or one seat class, small by
// nature, so under this many rows its error is the few beads' own time rather
// than a figure an operator would read.
const storyPointsEvalMinGroup = 5

// EvalSplit is one group of the test part a split prints, carrying the same
// predictor figures the base table does when it holds storyPointsEvalMinGroup
// rows or more and nil figures when it holds fewer.
type EvalSplit struct {
	Name  string
	Rows  int
	Stats []PredictorStats
}

// runStoryPointsEval is main's storypoints-eval step: the join over the wired
// town, split into the part the figures are read from, and printed to Out.
func (c Commands) runStoryPointsEval(real RealTown) error {
	since, err := storyPointsEvalSince(c.StoryPointsSince)
	if err != nil {
		return err
	}
	rows, counts, err := joinEval(real.Town)
	if err != nil {
		return err
	}
	train, test := splitEval(rows, since)
	enough := len(test) >= storyPointsEvalMinTest
	var stats []PredictorStats
	if enough {
		stats = evalStats(train, test)
	}
	if err := writeStoryPointsEval(c.Out, counts, len(test), stats); err != nil {
		return err
	}
	if !enough {
		// The base table printed no figure from a part too small to read, so
		// the splits print none either, from groups smaller still.
		return nil
	}
	if err := writeEvalSplits(c.Out, "by review rounds (the test part)", evalRoundSplits(train, test)); err != nil {
		return err
	}
	return writeEvalSplits(c.Out, "by seat class (the test part)", evalSeatSplits(train, test))
}

// storyPointsEvalSince reads the --since value. Empty is the zero time, which
// splitEval reads as the newest three rows in ten. A value that is not a
// YYYY-MM-DD day is an error, not the zero time: a typo would otherwise print
// the default split as though it were the day asked for.
func storyPointsEvalSince(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	day, err := time.Parse(time.DateOnly, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q is not a YYYY-MM-DD date", value)
	}
	return day, nil
}

// writeStoryPointsEval prints the evaluation: the counts the join gives, then,
// when the test part holds storyPointsEvalMinTest rows, one row per predictor
// with its held-out error. stats is nil when the part is under the minimum, so
// no figure is printed from a part too small to read.
func writeStoryPointsEval(w io.Writer, counts EvalCounts, testRows int, stats []PredictorStats) error {
	if _, err := fmt.Fprintf(w, "story points evaluation: %d test rows of %d joined rows (%d landing records read)\n"+
		"  dropped: no estimate %d, lower bound %d, upper bound %d, absent %d; malformed log lines %d\n",
		testRows, counts.Joined, counts.Records,
		counts.NoEstimate, counts.LowerBound, counts.UpperBound, counts.Absent, counts.MalformedLog); err != nil {
		return err
	}
	if testRows < storyPointsEvalMinTest {
		_, err := fmt.Fprintf(w, "  the test part is under %d rows, so no error figures are printed\n", storyPointsEvalMinTest)
		return err
	}
	_, err := io.WriteString(w, predictorTable("", stats))
	return err
}

// predictorTable is the predictor table as text: the header, then one row per
// predictor. indent prefixes every line so a table under a group heading stands
// inside it. The table is built in a strings.Builder, which cannot fail.
func predictorTable(indent string, stats []PredictorStats) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	writePredictorRows(tw, indent, stats)
	_ = tw.Flush()
	return b.String()
}

// writePredictorRows writes the predictor table to tw, whose writes cannot fail
// because it wraps a strings.Builder.
func writePredictorRows(tw *tabwriter.Writer, indent string, stats []PredictorStats) {
	fmt.Fprintf(tw, "%spredictor\tn\tMAE (min)\tmedian AE (min)\twithin 1.5x\trank rho\tlog n\tlog RMS\tlog bias\n", indent)
	for _, s := range stats {
		logN := s.N - s.SkippedLog
		fmt.Fprintf(tw, "%s%s\t%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			indent, s.Name, s.N,
			evalFigure(s.N != 0, s.MAE),
			evalFigure(s.N != 0, s.MedianAE),
			fmtShare(s.Within, s.WithinOK),
			evalRank(s),
			logN,
			evalFigure(logN != 0, s.LogRMS),
			evalFigure(logN != 0, s.LogBias))
	}
}

// writeEvalSplits writes one split of the evaluation (gt-he1f8o): the section
// title, then for each group its test row count and, when the group holds
// storyPointsEvalMinGroup rows or more, the same predictor table the evaluation
// prints. A group under the minimum prints its count alone, never a figure from
// the few beads it holds.
//
// The section is built in a strings.Builder and written once, so a refused
// write is the run's error and a section half printed does not read as a whole.
func writeEvalSplits(w io.Writer, title string, splits []EvalSplit) error {
	var b strings.Builder
	// One tabwriter over the whole section aligns every group's table alike.
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "\n%s\n", title)
	for _, s := range splits {
		fmt.Fprintf(tw, "  %s: %s\n", s.Name, evalRowCount(s.Rows))
		if s.Stats == nil {
			fmt.Fprintf(tw, "    under %d test rows, so no figures\n", storyPointsEvalMinGroup)
			continue
		}
		writePredictorRows(tw, "    ", s.Stats)
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, b.String())
	return err
}

// evalRowCount is a row count with its noun, so a group of one reads "1 test
// row" rather than "1 test rows".
func evalRowCount(n int) string {
	if n == 1 {
		return "1 test row"
	}
	return fmt.Sprintf("%d test rows", n)
}

// evalRoundSplits groups the test rows by the review rounds their landing
// record carries (gt-hx18i9): one, two and three or more. A record with no
// rounds is neither — the landing record leaves zero for a bead with no review
// rows and for a review ledger it could not read — so those rows are counted in
// a group of their own, named for what the record holds.
func evalRoundSplits(train, test []EvalRow) []EvalSplit {
	buckets := []struct {
		name string
		in   func(rounds int) bool
	}{
		{"1 round", func(rounds int) bool { return rounds == 1 }},
		{"2 rounds", func(rounds int) bool { return rounds == 2 }},
		{"3+ rounds", func(rounds int) bool { return rounds >= 3 }},
		{"no rounds recorded", func(rounds int) bool { return rounds <= 0 }},
	}
	splits := make([]EvalSplit, 0, len(buckets))
	for _, bucket := range buckets {
		var rows []EvalRow
		for _, row := range test {
			if bucket.in(row.OMRounds) {
				rows = append(rows, row)
			}
		}
		if len(rows) == 0 {
			continue
		}
		splits = append(splits, evalSplitOf(bucket.name, train, rows))
	}
	return splits
}

// evalSeatSplits groups the test rows by the seat class of the agent their
// landing record carries, in class name order. The classes are the ones the
// rows hold, so a class storypoints.SeatClass learns later is never dropped
// from the table; a record with no agent is the group "(no seat class)".
func evalSeatSplits(train, test []EvalRow) []EvalSplit {
	byClass := map[string][]EvalRow{}
	for _, row := range test {
		class := storypoints.SeatClass(row.Agent)
		if class == "" {
			class = "(no seat class)"
		}
		byClass[class] = append(byClass[class], row)
	}
	names := make([]string, 0, len(byClass))
	for name := range byClass {
		names = append(names, name)
	}
	slices.Sort(names)
	splits := make([]EvalSplit, 0, len(names))
	for _, name := range names {
		splits = append(splits, evalSplitOf(name, train, byClass[name]))
	}
	return splits
}

// evalSplitOf is one group's entry: its rows and, at storyPointsEvalMinGroup
// rows or more, the figures of the same predictors the base table holds, read
// over the group. The constant predictor keeps the training part's median, so
// every group is scored against the same baseline as the whole.
func evalSplitOf(name string, train, rows []EvalRow) EvalSplit {
	split := EvalSplit{Name: name, Rows: len(rows)}
	if len(rows) >= storyPointsEvalMinGroup {
		split.Stats = evalStats(train, rows)
	}
	return split
}

// evalFigure is one figure as the table prints it: two decimals, or n/a when
// defined is false, so a figure no row could produce never reads as zero.
func evalFigure(defined bool, v float64) string {
	if !defined {
		return "n/a"
	}
	if v == 0 {
		// The negative zero a log bias can carry prints as 0.00, not -0.00.
		v = 0
	}
	return fmt.Sprintf("%.2f", v)
}

// evalRank is a predictor's rank correlation, or n/a when RhoErr says it has
// none: too few rows, or no spread in one of the series.
func evalRank(s PredictorStats) string {
	if s.RhoErr != nil {
		return "n/a"
	}
	return evalFigure(true, s.Rho)
}
