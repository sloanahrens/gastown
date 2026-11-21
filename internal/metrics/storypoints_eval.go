package metrics

import (
	"encoding/json"
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

// storyPointsEvalCapMinutes is the cap the capped table applies to every
// polecat time (gt-xwqe1r): one first attempt of 103 hours (gt-8r17fp) would
// otherwise dominate the mean errors the base table prints.
const storyPointsEvalCapMinutes = 90

// EvalSplit is one group of the test part a split prints, carrying the same
// predictor figures the base table does when it holds storyPointsEvalMinGroup
// rows or more and nil figures when it holds fewer.
type EvalSplit struct {
	Name  string
	Rows  int
	Stats []PredictorStats
}

// runStoryPointsEval is main's storypoints-eval step: the join over the wired
// town, split into the part the figures are read from, printed to Out as the
// base table, the same table over the capped polecat times (gt-xwqe1r) and the
// splits. With Commands.StoryPointsJSON the same evaluation is printed as one
// JSON object instead of the text tables.
func (c Commands) runStoryPointsEval(real RealTown) error {
	since, err := storyPointsEvalSince(c.StoryPointsSince)
	if err != nil {
		return err
	}
	rows, counts, err := joinEval(real.Town)
	if err != nil {
		return err
	}
	report := newEvalReport(rows, counts, since)
	if c.StoryPointsJSON {
		return writeStoryPointsEvalJSON(c.Out, report)
	}
	if err := writeStoryPointsEval(c.Out, report.Counts, report.TestRows, report.Base); err != nil {
		return err
	}
	if report.Base == nil {
		// The base table printed no figure from a part too small to read, so
		// the capped table and the splits print none either, from a part or
		// groups smaller still.
		return nil
	}
	if err := writeEvalCapped(c.Out, report.Capped); err != nil {
		return err
	}
	if err := writeEvalSplits(c.Out, "by review rounds (the test part)", report.ByRounds); err != nil {
		return err
	}
	return writeEvalSplits(c.Out, "by seat class (the test part)", report.BySeat)
}

// evalReport is one evaluation's counts and tables, decided once so the text
// tables and the --json object print the same figures. Base, Capped, ByRounds
// and BySeat are nil when the test part is under storyPointsEvalMinTest rows: a
// part too small to read carries no figures in either form.
type evalReport struct {
	Counts   EvalCounts
	TestRows int
	Base     []PredictorStats
	Capped   []PredictorStats
	ByRounds []EvalSplit
	BySeat   []EvalSplit
}

// newEvalReport splits rows at since and scores every table the command prints
// from the parts they are read from.
func newEvalReport(rows []EvalRow, counts EvalCounts, since time.Time) evalReport {
	train, test := splitEval(rows, since)
	report := evalReport{Counts: counts, TestRows: len(test)}
	if len(test) < storyPointsEvalMinTest {
		return report
	}
	report.Base = evalStats(train, test)
	report.Capped = evalStats(capEvalRows(train), capEvalRows(test))
	report.ByRounds = evalRoundSplits(train, test)
	report.BySeat = evalSeatSplits(train, test)
	return report
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

// capEvalRows returns rows with every polecat time above
// storyPointsEvalCapMinutes lowered to it. Only the measurement changes: both
// tables score the same rows, so a difference between them is the cap alone.
func capEvalRows(rows []EvalRow) []EvalRow {
	capped := slices.Clone(rows)
	for i := range capped {
		if capped[i].ActualMinutes > storyPointsEvalCapMinutes {
			capped[i].ActualMinutes = storyPointsEvalCapMinutes
		}
	}
	return capped
}

// writeEvalCapped writes the base table again over the same test part with
// every polecat time capped at storyPointsEvalCapMinutes (gt-xwqe1r). The
// heading and the actual-minutes columns name the cap, so neither table reads
// as the other. The section is built in a strings.Builder and written once, so
// a refused write is the run's error and a section half printed does not read
// as a whole.
func writeEvalCapped(w io.Writer, stats []PredictorStats) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\nthe same table with every polecat time capped at %d minutes (the test part)\n", storyPointsEvalCapMinutes)
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	writeCappedPredictorRows(tw, "", stats)
	_ = tw.Flush()
	_, err := io.WriteString(w, b.String())
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

// writePredictorRows writes the predictor table measured against the polecat
// minutes as landed to tw, whose writes cannot fail because it wraps a
// strings.Builder.
func writePredictorRows(tw *tabwriter.Writer, indent string, stats []PredictorStats) {
	writePredictorHeader(tw, indent, "min")
	writePredictorStatRows(tw, indent, stats)
}

// writeCappedPredictorRows writes the predictor table measured against the
// capped polecat minutes, its header saying so.
func writeCappedPredictorRows(tw *tabwriter.Writer, indent string, stats []PredictorStats) {
	writePredictorHeader(tw, indent, fmt.Sprintf("min, capped at %d", storyPointsEvalCapMinutes))
	writePredictorStatRows(tw, indent, stats)
}

// writePredictorHeader writes the table's header row, actual naming the
// minutes column the errors are measured against.
func writePredictorHeader(tw *tabwriter.Writer, indent, actual string) {
	fmt.Fprintf(tw, "%spredictor\tn\tMAE (%s)\tmedian AE (%s)\twithin 1.5x\trank rho\tlog n\tlog RMS\tlog bias\n", indent, actual, actual)
}

// writePredictorStatRows writes one row per predictor under the header.
func writePredictorStatRows(tw *tabwriter.Writer, indent string, stats []PredictorStats) {
	for _, s := range stats {
		row := evalRowOf(s)
		fmt.Fprintf(tw, "%s%s\t%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			indent, s.Name, row.n,
			row.mae.text(), row.medianAE.text(), row.within.text(), row.rho.text(),
			row.logN, row.logRMS.text(), row.logBias.text())
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

// evalColumn is one figure of a predictor's table row: the value the row holds
// and whether it holds one at all. The text table prints an undefined column as
// n/a and the --json object as null, both from this one decision, so the two
// forms never disagree about a row's figures.
type evalColumn struct {
	defined bool
	value   float64
}

// text is the cell the text table prints: two decimals, or n/a when the row has
// no figure, so a figure no row could produce never reads as zero.
func (c evalColumn) text() string {
	if !c.defined {
		return "n/a"
	}
	if c.value == 0 {
		// The negative zero a log bias can carry prints as 0.00, not -0.00.
		c.value = 0
	}
	return fmt.Sprintf("%.2f", c.value)
}

// number is the figure the --json object carries: the same literal the text
// cell prints, so a reader compares the object with the table figure for
// figure. It is nil for n/a, never zero.
func (c evalColumn) number() *json.Number {
	if !c.defined {
		return nil
	}
	n := json.Number(c.text())
	return &n
}

// evalRowFigures is one predictor's row: the count and the columns it is scored
// on. Both forms of the table read a row through this, so a column with no
// figure is n/a as text and null in the JSON object.
type evalRowFigures struct {
	n        int
	mae      evalColumn
	medianAE evalColumn
	within   evalColumn
	rho      evalColumn
	logN     int
	logRMS   evalColumn
	logBias  evalColumn
}

// evalRowOf decides one predictor's row. A predictor that scored no test row has
// neither the error figures nor the log ones, and a rank correlation with no
// spread in either series has none (RhoErr).
func evalRowOf(s PredictorStats) evalRowFigures {
	logN := s.N - s.SkippedLog
	return evalRowFigures{
		n:        s.N,
		mae:      evalColumn{s.N != 0, s.MAE},
		medianAE: evalColumn{s.N != 0, s.MedianAE},
		within:   evalColumn{s.WithinOK, s.Within},
		rho:      evalColumn{s.RhoErr == nil, s.Rho},
		logN:     logN,
		logRMS:   evalColumn{logN != 0, s.LogRMS},
		logBias:  evalColumn{logN != 0, s.LogBias},
	}
}

// The --json object (gt-99x14w): the counts and the four tables the text prints,
// as one document whose figures are the text tables' own. A figure the text
// prints as n/a is null, and a table the text does not print is null too, so a
// reader is never handed a zero for a figure no row could produce.

// evalJSONReport is the object storypoints-eval --json prints.
type evalJSONReport struct {
	// TestRows is the size of the test part every table is read from.
	TestRows int `json:"test_rows"`
	// Counts is the join's accounting, the text's opening line.
	Counts evalJSONCounts `json:"counts"`
	// Base is the table over the test part as landed, and Capped the same table
	// over the capped polecat times (gt-xwqe1r). Both are null when the test
	// part is under storyPointsEvalMinTest rows, where the text prints none.
	Base   *evalJSONTable       `json:"base"`
	Capped *evalJSONCappedTable `json:"capped"`
	// ByRounds and BySeat are the splits (gt-he1f8o), null in the same short
	// part.
	ByRounds []evalJSONSplit `json:"by_review_rounds"`
	BySeat   []evalJSONSplit `json:"by_seat_class"`
}

// evalJSONCounts is the join's accounting: every landing record read, how many
// joined, and the reasons the rest were dropped.
type evalJSONCounts struct {
	Records      int `json:"records"`
	Joined       int `json:"joined"`
	NoEstimate   int `json:"no_estimate"`
	LowerBound   int `json:"lower_bound"`
	UpperBound   int `json:"upper_bound"`
	Absent       int `json:"absent"`
	MalformedLog int `json:"malformed_log"`
}

// evalJSONTable is one table of predictor rows.
type evalJSONTable struct {
	Predictors []evalJSONPredictor `json:"predictors"`
}

// evalJSONCappedTable is the capped table: its figures and the minute cap they
// were scored at.
type evalJSONCappedTable struct {
	CapMinutes int                 `json:"cap_minutes"`
	Predictors []evalJSONPredictor `json:"predictors"`
}

// evalJSONSplit is one group of a split: its name as the text heads it, the test
// rows it holds, and the predictor rows. Predictors is null for a group under
// storyPointsEvalMinGroup rows, which the text prints as a count alone.
type evalJSONSplit struct {
	Name       string              `json:"name"`
	Rows       int                 `json:"rows"`
	Predictors []evalJSONPredictor `json:"predictors"`
}

// evalJSONPredictor is one predictor's row: the columns the text table prints,
// each figure the same literal, and null where the text prints n/a.
type evalJSONPredictor struct {
	Name     string       `json:"name"`
	N        int          `json:"n"`
	MAE      *json.Number `json:"mae"`
	MedianAE *json.Number `json:"median_ae"`
	Within   *json.Number `json:"within_1_5x"`
	Rho      *json.Number `json:"rank_rho"`
	LogN     int          `json:"log_n"`
	LogRMS   *json.Number `json:"log_rms"`
	LogBias  *json.Number `json:"log_bias"`
}

// writeStoryPointsEvalJSON prints the evaluation as one JSON object: the counts
// and the four tables the text prints, each figure the one the text shows. The
// write is the run's, so one the encoder cannot make fails the run rather than
// reporting an evaluation nobody received.
func writeStoryPointsEvalJSON(w io.Writer, report evalReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(evalJSONOf(report)); err != nil {
		return fmt.Errorf("print the evaluation as JSON: %w", err)
	}
	return nil
}

// evalJSONOf is the object for one evaluation, read from the report the text
// tables are printed from so both carry the same figures.
func evalJSONOf(report evalReport) evalJSONReport {
	doc := evalJSONReport{
		TestRows: report.TestRows,
		Counts: evalJSONCounts{
			Records:      report.Counts.Records,
			Joined:       report.Counts.Joined,
			NoEstimate:   report.Counts.NoEstimate,
			LowerBound:   report.Counts.LowerBound,
			UpperBound:   report.Counts.UpperBound,
			Absent:       report.Counts.Absent,
			MalformedLog: report.Counts.MalformedLog,
		},
	}
	if report.Base == nil {
		return doc
	}
	doc.Base = &evalJSONTable{Predictors: evalJSONPredictors(report.Base)}
	doc.Capped = &evalJSONCappedTable{
		CapMinutes: storyPointsEvalCapMinutes,
		Predictors: evalJSONPredictors(report.Capped),
	}
	doc.ByRounds = evalJSONSplits(report.ByRounds)
	doc.BySeat = evalJSONSplits(report.BySeat)
	return doc
}

// evalJSONSplits is the splits' rows, a group's figures null when it printed
// none.
func evalJSONSplits(splits []EvalSplit) []evalJSONSplit {
	out := make([]evalJSONSplit, 0, len(splits))
	for _, s := range splits {
		out = append(out, evalJSONSplit{
			Name:       s.Name,
			Rows:       s.Rows,
			Predictors: evalJSONPredictors(s.Stats),
		})
	}
	return out
}

// evalJSONPredictors is a table's predictor rows, or nil for a part or group
// whose stats the text does not print.
func evalJSONPredictors(stats []PredictorStats) []evalJSONPredictor {
	if stats == nil {
		return nil
	}
	out := make([]evalJSONPredictor, 0, len(stats))
	for _, s := range stats {
		row := evalRowOf(s)
		out = append(out, evalJSONPredictor{
			Name:     s.Name,
			N:        row.n,
			MAE:      row.mae.number(),
			MedianAE: row.medianAE.number(),
			Within:   row.within.number(),
			Rho:      row.rho.number(),
			LogN:     row.logN,
			LogRMS:   row.logRMS.number(),
			LogBias:  row.logBias.number(),
		})
	}
	return out
}
