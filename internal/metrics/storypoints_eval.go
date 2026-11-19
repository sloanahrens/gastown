package metrics

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// The story-points evaluation step (gt-htz2cb): the held-out error of the
// estimate over the town's landed beads, printed over the test part the split
// names. The join is gt-mo0p0v, the split and the statistics gt-wizcsy.

// storyPointsEvalMinTest is the fewest test rows the error figures are printed
// from. Under it the part is too small to read, so the step prints the join's
// counts alone rather than a figure an operator would trust.
const storyPointsEvalMinTest = 30

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
	var stats []PredictorStats
	if len(test) >= storyPointsEvalMinTest {
		stats = evalStats(train, test)
	}
	return writeStoryPointsEval(c.Out, counts, len(test), stats)
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
	if _, err := fmt.Fprintf(w, "story points evaluation: %d test rows of %d joined rows (%d landing records read)\n",
		testRows, counts.Joined, counts.Records); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  dropped: no estimate %d, lower bound %d, upper bound %d, absent %d; malformed log lines %d\n",
		counts.NoEstimate, counts.LowerBound, counts.UpperBound, counts.Absent, counts.MalformedLog); err != nil {
		return err
	}
	if testRows < storyPointsEvalMinTest {
		_, err := fmt.Fprintf(w, "  the test part is under %d rows, so no error figures are printed\n", storyPointsEvalMinTest)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "predictor\tn\tMAE (min)\tmedian AE (min)\twithin 1.5x\trank rho\tlog n\tlog RMS\tlog bias")
	for _, s := range stats {
		logN := s.N - s.SkippedLog
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			s.Name, s.N,
			evalFigure(s.N != 0, s.MAE),
			evalFigure(s.N != 0, s.MedianAE),
			fmtShare(s.Within, s.WithinOK),
			evalRank(s),
			logN,
			evalFigure(logN != 0, s.LogRMS),
			evalFigure(logN != 0, s.LogBias))
	}
	return tw.Flush()
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
