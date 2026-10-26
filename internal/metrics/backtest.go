package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The usefulness backtest: whether a metric moved at a regime event nobody told
// it about, with a mango box that replicates and Sloan's answer as the other
// evidence for keeping it. It is a port of scripts/town_metrics/backtest.py, so
// the same weekly rows and events give the same verdicts.

// backtestMetric is one metric and the weekly columns that carry it, in the
// order the report lists them.
type backtestMetric struct {
	name    string
	columns []string
}

var backtestMetrics = []backtestMetric{
	{"landing size", []string{"size_median", "size_p90"}},
	{"file and package entropy", []string{"file_entropy_mean", "package_entropy_mean"}},
	{"mode shift", []string{"mode_shift"}},
	{"rework depth", []string{"attempts_mean", "rejected_share"}},
	{"label entropy", []string{"h_labels", "h_rigs", "h_types"}},
	{"handoff size", []string{"handoff_bytes_total", "handoff_bytes_median"}},
	{"rho", []string{"rho"}},
	{"dependency density", []string{"dependency_density"}},
	{"non-conservative merges", []string{"conflict_gate_share"}},
}

// landingDerived marks the columns the extract computes from landings. A thin
// week has too few landings for them to mean anything; a bead-derived column is
// measured on beads, so the landing count does not thin it.
var landingDerived = map[string]bool{
	"size_median": true, "size_p90": true, "file_entropy_mean": true,
	"package_entropy_mean": true, "attempts_mean": true, "rejected_share": true,
	"first_try_share": true, "manual_rescue_count": true, "rejections": true,
	"conflict_gate_share": true, "unknown_share": true, "mode_shift": true,
	"rho": true, "rho_numerator": true, "rho_denominator": true,
}

const (
	// backtestMinChanges is how many comparable week-to-week changes a series
	// needs before a move or a chance is reported at all.
	backtestMinChanges = 5
	// weakAbove is the chance of coincidence above which a move at an event is
	// a prompt to look rather than evidence for keeping the metric.
	weakAbove = 0.25
	// backtestThreshold and backtestMinRows are correlated_pairs' defaults: a
	// pair is reported above this |r| over at least this many shared rows.
	backtestThreshold = 0.9
	backtestMinRows   = 6
)

// The backtest's verdicts: a metric stays while any evidence holds, goes only
// when every source has answered no, and is undecided while a source is
// unanswered.
const (
	BacktestKeep      = "keep"
	BacktestDrop      = "drop"
	BacktestUndecided = "undecided"
)

// Answer is a yes, a no or an unanswered question: nil is the Python's None.
type Answer = *bool

// WeeklyRow is one week of one rig's metrics as weekly.csv carries them, with
// the extract's ALL row beside each rig's. Values holds the numeric columns; a
// column the week has no value for is absent from the map, which is the
// Python's None.
type WeeklyRow struct {
	Week   string
	Rig    string
	Thin   bool
	Values map[string]float64
}

// value returns the row's value for column and whether the week has one.
func (r WeeklyRow) value(column string) (float64, bool) {
	v, ok := r.Values[column]
	return v, ok
}

// SeriesPoint is one week of a metric's series.
type SeriesPoint struct {
	Week  string
	Value float64
}

// RegimeEvent is one entry of regime-events.json: the day a regime changed and
// what changed, unconfirmed until the operator confirms it.
type RegimeEvent struct {
	Date      string `json:"date"`
	What      string `json:"what"`
	Confirmed bool   `json:"confirmed"`
}

// MangoSummary is one target's line in mango-summary.json: the boxes a run
// built, how many replicated on the holdout, and how many holdout positives
// there were to check them on.
type MangoSummary struct {
	Boxes            int `json:"boxes"`
	Replicating      int `json:"replicating"`
	HoldoutPositives int `json:"holdout_positives"`
}

// targetMetric maps a mango target to the metric a box on it speaks for: the
// post-hoc columns only explain boxes, so the target itself is the metric.
// Every target here is one mango runs, which the test holds against
// mangoTargets; a target mango dropped answers nothing about its metric.
var targetMetric = []struct{ target, metric string }{
	{"rejected_before_landing", "rework depth"},
}

// CorrelatedPair is two weekly columns whose values move together, and how
// closely.
type CorrelatedPair struct {
	A, B string
	R    float64
}

// Series returns column's ALL-rig values in week order, leaving out the weeks a
// landing-derived column cannot measure because the week was thin.
func Series(weeklyAll []WeeklyRow, column string) []SeriesPoint {
	var rows []WeeklyRow
	for _, r := range weeklyAll {
		if r.Rig == "ALL" {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Week < rows[j].Week })
	var out []SeriesPoint
	for _, r := range rows {
		v, ok := r.value(column)
		if !ok || (landingDerived[column] && r.Thin) {
			continue
		}
		out = append(out, SeriesPoint{Week: r.Week, Value: v})
	}
	return out
}

// MovesAtEvent reports whether the change into the week of eventDate is beyond
// two standard deviations of the series' other week-to-week changes; the answer
// is nil when the series has fewer than minChanges comparable changes, the event
// week is not in it, or the history is too flat to say.
func MovesAtEvent(weeklyAll []WeeklyRow, column, eventDate string, minChanges int) (Answer, error) {
	eventWeek, err := WeekOf(eventDate + "T00:00:00Z")
	if err != nil {
		return nil, fmt.Errorf("event date %q: %w", eventDate, err)
	}
	changes, err := weeklyChanges(weeklyAll, column)
	if err != nil {
		return nil, err
	}
	byWeek := make(map[string]float64, len(changes))
	for _, c := range changes {
		byWeek[c.week] = c.delta
	}
	change, ok := byWeek[eventWeek]
	if !ok || len(byWeek) < minChanges {
		return nil, nil
	}
	var others []float64
	for week, delta := range byWeek {
		if week != eventWeek {
			others = append(others, delta)
		}
	}
	sd := 0.0
	if len(others) > 1 {
		sd = pstdev(others)
	}
	if sd == 0 {
		return nil, nil
	}
	moved := math.Abs(change) > 2*sd
	return &moved, nil
}

// ChanceOfCoincidence is the chance that the series' single largest move falls
// in one of the event weeks by luck: event weeks divided by comparable changes,
// nil when the series has fewer than minChanges of them.
func ChanceOfCoincidence(weeklyAll []WeeklyRow, column string, events []RegimeEvent, minChanges int) (*float64, error) {
	changes, err := weeklyChanges(weeklyAll, column)
	if err != nil {
		return nil, err
	}
	if len(changes) < minChanges {
		return nil, nil
	}
	eventWeeks := make(map[string]bool, len(events))
	for _, e := range events {
		week, err := WeekOf(e.Date + "T00:00:00Z")
		if err != nil {
			return nil, fmt.Errorf("event date %q: %w", e.Date, err)
		}
		eventWeeks[week] = true
	}
	hit := make(map[string]bool, len(eventWeeks))
	for _, c := range changes {
		if eventWeeks[c.week] {
			hit[c.week] = true
		}
	}
	chance := float64(len(hit)) / float64(len(changes))
	return &chance, nil
}

// CorrelatedPairs returns the column pairs whose values correlate above
// threshold over at least minRows rows where both are present and not thin.
func CorrelatedPairs(weekly []WeeklyRow, columns []string, threshold float64, minRows int) []CorrelatedPair {
	var out []CorrelatedPair
	for i, a := range columns {
		for _, b := range columns[i+1:] {
			var xs, ys []float64
			for _, r := range weekly {
				x, okX := r.value(a)
				y, okY := r.value(b)
				if !okX || !okY || r.Thin {
					continue
				}
				xs = append(xs, x)
				ys = append(ys, y)
			}
			if len(xs) < minRows {
				continue
			}
			r := pearson(xs, ys)
			if r != nil && math.Abs(*r) > threshold {
				out = append(out, CorrelatedPair{A: a, B: b, R: *r})
			}
		}
	}
	return out
}

// BacktestVerdict is the backtest's verdict on a metric: keep while any source
// says yes, drop only when every source has answered no, undecided otherwise.
//
// The name carries its stage because Verdict in panel.go already names the
// mango panel's per-target answer.
func BacktestVerdict(moved, replicated, sloanWouldAct Answer) string {
	if isTrue(moved) || isTrue(replicated) || isTrue(sloanWouldAct) {
		return BacktestKeep
	}
	if isFalse(moved) && isFalse(replicated) && isFalse(sloanWouldAct) {
		return BacktestDrop
	}
	return BacktestUndecided
}

// BacktestSection renders the backtest as the report's Markdown: one table row
// per metric with the evidence for keeping it, then the notes that keep a
// verdict from reading as more than it is.
func BacktestSection(weekly []WeeklyRow, events []RegimeEvent, replicated, sloan map[string]Answer) (string, error) {
	var allRows []WeeklyRow
	for _, r := range weekly {
		if r.Rig == "ALL" {
			allRows = append(allRows, r)
		}
	}
	lines := []string{eventsLine(events), ""}
	header := []string{"Metric", "moved at event", "chance of coincidence", "mango box replicates", "Sloan would act", "verdict"}
	table := []string{
		"| " + strings.Join(header, " | ") + " |",
		"|" + strings.Repeat("---|", len(header)),
	}
	short := false
	for _, m := range backtestMetrics {
		moved, err := metricMoved(allRows, m.columns, events)
		if err != nil {
			return "", fmt.Errorf("metric %q: %w", m.name, err)
		}
		short = short || moved == nil
		worst, err := worstChance(allRows, m.columns, events)
		if err != nil {
			return "", fmt.Errorf("metric %q: %w", m.name, err)
		}
		weak := isTrue(moved) && worst != nil && *worst > weakAbove
		shown, counted := answerCell(moved), moved
		if weak {
			shown, counted = "yes (weak)", nil
		}
		table = append(table, "| "+strings.Join([]string{
			m.name,
			shown,
			percentCell(worst),
			answerCell(replicated[m.name]),
			answerCell(sloan[m.name]),
			BacktestVerdict(counted, replicated[m.name], sloan[m.name]),
		}, " | ")+" |")
	}
	lines = append(lines, table...)
	lines = append(lines, "", weakEvidenceNote)
	if short {
		lines = append(lines, "", insufficientHistoryNote)
	}
	lines = append(lines, "", pairsLine(weekly))
	return strings.Join(lines, "\n"), nil
}

// ReplicatedFromSummary reports, per metric, whether its mango target had boxes
// and a holdout with positives to check them on; a target without both is
// absent.
func ReplicatedFromSummary(summary map[string]MangoSummary) map[string]Answer {
	out := map[string]Answer{}
	for _, tm := range targetMetric {
		s := summary[tm.target]
		if s.Boxes != 0 && s.HoldoutPositives != 0 {
			replicated := s.Replicating > 0
			out[tm.metric] = &replicated
		}
	}
	return out
}

// ReadBacktestInputs reads regime-events.json and sloan-answers.json from
// docsDir through env.ReadFile; a file that is missing or does not parse
// contributes nothing, so a hand-edited file cannot stop the report.
func ReadBacktestInputs(env Env, docsDir string) (events []RegimeEvent, sloan map[string]Answer) {
	if data, err := env.ReadFile(filepath.Join(docsDir, "regime-events.json")); err == nil {
		_ = json.Unmarshal(data, &events)
	}
	if data, err := env.ReadFile(filepath.Join(docsDir, "sloan-answers.json")); err == nil {
		_ = json.Unmarshal(data, &sloan)
	}
	return events, sloan
}

// weakEvidenceNote and insufficientHistoryNote are the report's two readings of
// its own table, kept verbatim from the Python so a published report does not
// change wording with its implementation.
const (
	weakEvidenceNote        = "A yes under moved-at-event is weak evidence when the chance of coincidence is high: with few weekly changes and several events, the series' biggest move lands on an event week by luck that often. Read a verdict as a prompt to look, not as a result."
	insufficientHistoryNote = `n/a under "moved at event" means insufficient history: fewer than 5 comparable week-to-week changes, an event outside the series, or a flat history. It is not a no.`
)

// weeklyChange is one comparable week-to-week change in a series.
type weeklyChange struct {
	week  string
	delta float64
}

// weeklyChanges returns each comparable change in column's ALL-rig series, in
// week order. A change over any gap other than a week is not a week-to-week
// change, so a gap in the series is not read as a move.
func weeklyChanges(weeklyAll []WeeklyRow, column string) ([]weeklyChange, error) {
	points := Series(weeklyAll, column)
	var changes []weeklyChange
	for i := 1; i < len(points); i++ {
		d, err := days(points[i-1].Week, points[i].Week)
		if err != nil {
			return nil, err
		}
		if d != 7 {
			continue
		}
		changes = append(changes, weeklyChange{week: points[i].Week, delta: points[i].Value - points[i-1].Value})
	}
	return changes, nil
}

// days is the number of days from a to b, both YYYY-MM-DD.
func days(a, b string) (int, error) {
	from, err := time.Parse("2006-01-02", a)
	if err != nil {
		return 0, fmt.Errorf("week %q: %w", a, err)
	}
	to, err := time.Parse("2006-01-02", b)
	if err != nil {
		return 0, fmt.Errorf("week %q: %w", b, err)
	}
	return int(to.Sub(from) / (24 * time.Hour)), nil
}

// pearson is the Pearson correlation of xs and ys, nil when either side is
// constant and the correlation is undefined.
func pearson(xs, ys []float64) *float64 {
	n := len(xs)
	if len(ys) < n {
		n = len(ys)
	}
	if n == 0 {
		return nil
	}
	mx, my := fmean(xs[:n]), fmean(ys[:n])
	var sx, sy, sxy float64
	for i := 0; i < n; i++ {
		dx, dy := xs[i]-mx, ys[i]-my
		sx += dx * dx
		sy += dy * dy
		sxy += dx * dy
	}
	sx, sy = math.Sqrt(sx), math.Sqrt(sy)
	if sx == 0 || sy == 0 {
		return nil
	}
	r := sxy / (sx * sy)
	return &r
}

// fmean is statistics.fmean: the mean of values.
func fmean(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// pstdev is statistics.pstdev: the population standard deviation of values.
func pstdev(values []float64) float64 {
	mean := fmean(values)
	var sum float64
	for _, v := range values {
		sum += (v - mean) * (v - mean)
	}
	return math.Sqrt(sum / float64(len(values)))
}

// metricMoved is whether any of the metric's columns moved at any of the
// events: a yes on the first, a no only when every answer is a no, and nil when
// nothing answered.
func metricMoved(allRows []WeeklyRow, columns []string, events []RegimeEvent) (Answer, error) {
	var answers []Answer
	for _, c := range columns {
		for _, e := range events {
			moved, err := MovesAtEvent(allRows, c, e.Date, backtestMinChanges)
			if err != nil {
				return nil, err
			}
			answers = append(answers, moved)
		}
	}
	for _, a := range answers {
		if isTrue(a) {
			yes := true
			return &yes, nil
		}
	}
	if len(answers) == 0 {
		return nil, nil
	}
	for _, a := range answers {
		if !isFalse(a) {
			return nil, nil
		}
	}
	no := false
	return &no, nil
}

// worstChance is the highest chance of coincidence among the metric's columns,
// nil when no column has a series long enough to say.
func worstChance(allRows []WeeklyRow, columns []string, events []RegimeEvent) (*float64, error) {
	var worst *float64
	for _, c := range columns {
		chance, err := ChanceOfCoincidence(allRows, c, events, backtestMinChanges)
		if err != nil {
			return nil, err
		}
		if chance != nil && (worst == nil || *chance > *worst) {
			worst = chance
		}
	}
	return worst, nil
}

// eventsLine names each event by date and what changed, marking the ones the
// operator has not confirmed.
func eventsLine(events []RegimeEvent) string {
	parts := make([]string, len(events))
	for i, e := range events {
		parts[i] = e.Date + " " + e.What
		if !e.Confirmed {
			parts[i] += " [unconfirmed]"
		}
	}
	return "Events (from `docs/metrics/regime-events.json`): " + strings.Join(parts, "; ") + "."
}

// pairsLine names the column pairs the report counts as one metric.
func pairsLine(weekly []WeeklyRow) string {
	var columns []string
	for _, m := range backtestMetrics {
		columns = append(columns, m.columns...)
	}
	pairs := CorrelatedPairs(weekly, columns, backtestThreshold, backtestMinRows)
	line := "Pairs correlated above 0.9 (merged into one metric): "
	if len(pairs) == 0 {
		return line + "none."
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s ~ %s (r=%.2f)", p.A, p.B, p.R)
	}
	return line + strings.Join(parts, "; ") + "."
}

// percentCell is a chance as the report's whole-number percentage.
func percentCell(chance *float64) string {
	if chance == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", 100*(*chance))
}

// answerCell renders an answer as the report's cell.
func answerCell(a Answer) string {
	switch {
	case a == nil:
		return "n/a"
	case *a:
		return "yes"
	}
	return "no"
}

// isTrue reports whether a is a yes; an unanswered question is not.
func isTrue(a Answer) bool { return a != nil && *a }

// isFalse reports whether a is a no; an unanswered question is not.
func isFalse(a Answer) bool { return a != nil && !*a }
