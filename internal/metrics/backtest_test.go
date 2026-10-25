package metrics

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// backtestWeeks are the eight Monday weeks the Python suite builds its series
// on, in order.
var backtestWeeks = []string{
	"2026-09-07", "2026-09-14", "2026-09-21", "2026-09-28",
	"2026-10-05", "2026-10-12", "2026-10-19", "2026-10-26",
}

// weeklyRows builds one ALL-rig row per value, each carrying column's value, as
// the Python tests' rows() helper does.
func weeklyRows(column string, values ...float64) []WeeklyRow {
	rows := make([]WeeklyRow, len(values))
	for i, v := range values {
		rows[i] = WeeklyRow{Week: backtestWeeks[i], Rig: "ALL", Values: map[string]float64{column: v}}
	}
	return rows
}

// thinWeeks marks the rows thin in the order given.
func thinWeeks(rows []WeeklyRow, thin []bool) []WeeklyRow {
	for i := range rows {
		if i < len(thin) {
			rows[i].Thin = thin[i]
		}
	}
	return rows
}

// answer is a yes or a no for a table that passes tri-state answers.
func answer(v bool) Answer { return &v }

// sectionLine returns the first line of text starting with prefix.
func sectionLine(t *testing.T, text, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no line starting %q in:\n%s", prefix, text)
	return ""
}

func TestBacktestMovesAtEventJumpAtTheEventWeek(t *testing.T) {
	t.Parallel()
	r := weeklyRows("size_median", 10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1) // the jump lands in the week of 2026-10-19
	moved, err := MovesAtEvent(r, "size_median", "2026-10-21", backtestMinChanges)
	if err != nil {
		t.Fatalf("MovesAtEvent: %v", err)
	}
	if !isTrue(moved) {
		t.Errorf("MovesAtEvent = %v, want true", answerCell(moved))
	}
}

func TestBacktestMovesAtEventSameJumpInsideNormalNoise(t *testing.T) {
	t.Parallel()
	r := weeklyRows("size_median", 10, 30, 10, 30, 10, 30, 10, 30)
	moved, err := MovesAtEvent(r, "size_median", "2026-10-19", backtestMinChanges)
	if err != nil {
		t.Fatalf("MovesAtEvent: %v", err)
	}
	if !isFalse(moved) {
		t.Errorf("MovesAtEvent = %v, want false", answerCell(moved))
	}
}

func TestBacktestMovesAtEventTooFewChangesIsUnknown(t *testing.T) {
	t.Parallel()
	moved, err := MovesAtEvent(weeklyRows("size_median", 10, 11, 30), "size_median", "2026-09-22", backtestMinChanges)
	if err != nil {
		t.Fatalf("MovesAtEvent: %v", err)
	}
	if moved != nil {
		t.Errorf("MovesAtEvent = %v, want unknown", answerCell(moved))
	}
}

func TestBacktestMovesAtEventOutsideTheSeriesIsUnknown(t *testing.T) {
	t.Parallel()
	moved, err := MovesAtEvent(weeklyRows("size_median", 1, 2, 3, 4, 5, 6, 7, 8), "size_median", "2026-01-01", backtestMinChanges)
	if err != nil {
		t.Fatalf("MovesAtEvent: %v", err)
	}
	if moved != nil {
		t.Errorf("MovesAtEvent = %v, want unknown for an event before the series", answerCell(moved))
	}
}

func TestBacktestSeriesSkipsThinWeeksForLandingColumnsOnly(t *testing.T) {
	t.Parallel()
	thin := []bool{true, true, true, false, false, false, false, false}
	if got := len(Series(thinWeeks(weeklyRows("size_median", 1, 1, 1, 1, 1, 1, 1, 1), thin), "size_median")); got != 5 {
		t.Errorf("landing-derived series = %d weeks, want 5", got)
	}
	if got := len(Series(thinWeeks(weeklyRows("h_labels", 1, 1, 1, 1, 1, 1, 1, 1), thin), "h_labels")); got != 8 {
		t.Errorf("bead-derived series = %d weeks, want 8", got)
	}
}

func TestBacktestMovesAtEventFlatHistoryIsUnknown(t *testing.T) {
	t.Parallel()
	// every other week-to-week change is 0, so "two standard deviations" is 0 and the test says nothing
	r := weeklyRows("size_median", 5, 5, 5, 5, 5, 5, 9, 9)
	moved, err := MovesAtEvent(r, "size_median", "2026-10-19", backtestMinChanges)
	if err != nil {
		t.Fatalf("MovesAtEvent: %v", err)
	}
	if moved != nil {
		t.Errorf("MovesAtEvent = %v, want unknown on a flat history", answerCell(moved))
	}
}

func TestBacktestCorrelatedPairsFindsPerfectlyCorrelatedColumns(t *testing.T) {
	t.Parallel()
	r := make([]WeeklyRow, 8)
	for i := range r {
		r[i] = WeeklyRow{Week: fmt.Sprintf("w%d", i), Rig: "gastown", Values: map[string]float64{
			"a": float64(i), "b": 2*float64(i) + 1, "c": float64((i * 7) % 5),
		}}
	}
	pairs := CorrelatedPairs(r, []string{"a", "b", "c"}, backtestThreshold, backtestMinRows)
	if len(pairs) != 1 {
		t.Fatalf("CorrelatedPairs = %v, want only a ~ b", pairs)
	}
	if pairs[0].A != "a" || pairs[0].B != "b" {
		t.Errorf("pair = %s ~ %s, want a ~ b", pairs[0].A, pairs[0].B)
	}
	if math.Abs(pairs[0].R-1.0) > 1e-9 {
		t.Errorf("r = %v, want 1", pairs[0].R)
	}
}

func TestBacktestCorrelatedPairsSkipsFewSharedRows(t *testing.T) {
	t.Parallel()
	r := make([]WeeklyRow, 5)
	for i := range r {
		r[i] = WeeklyRow{Week: fmt.Sprintf("w%d", i), Rig: "x", Values: map[string]float64{
			"a": float64(i), "b": float64(i),
		}}
	}
	if pairs := CorrelatedPairs(r, []string{"a", "b"}, backtestThreshold, backtestMinRows); len(pairs) != 0 {
		t.Errorf("CorrelatedPairs = %v, want none over fewer than six shared rows", pairs)
	}
}

func TestBacktestCorrelatedPairsConstantColumnNeverCorrelates(t *testing.T) {
	t.Parallel()
	r := make([]WeeklyRow, 8)
	for i := range r {
		r[i] = WeeklyRow{Week: fmt.Sprintf("w%d", i), Rig: "x", Values: map[string]float64{
			"a": float64(i), "b": 1.0,
		}}
	}
	if pairs := CorrelatedPairs(r, []string{"a", "b"}, backtestThreshold, backtestMinRows); len(pairs) != 0 {
		t.Errorf("CorrelatedPairs = %v, want none for a constant column", pairs)
	}
}

func TestBacktestVerdictTruthTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                         string
		moved, replicated, sloanActs Answer
		want                         string
	}{
		{"a move keeps", answer(true), nil, nil, BacktestKeep},
		{"a replicated box keeps", answer(false), answer(true), nil, BacktestKeep},
		{"Sloan's yes keeps", answer(false), answer(false), answer(true), BacktestKeep},
		{"three noes drop", answer(false), answer(false), answer(false), BacktestDrop},
		{"no evidence yet is not evidence against", nil, answer(false), answer(false), BacktestUndecided},
		{"an unanswered box is undecided", answer(false), nil, answer(false), BacktestUndecided},
		{"Sloan not asked yet is undecided", answer(false), answer(false), nil, BacktestUndecided},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BacktestVerdict(tc.moved, tc.replicated, tc.sloanActs); got != tc.want {
				t.Errorf("BacktestVerdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBacktestSectionListsEveryMetric(t *testing.T) {
	t.Parallel()
	weekly := weeklyRows("size_median", 10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1)
	events := []RegimeEvent{{Date: "2026-10-21", What: "gate raised"}}
	text, err := BacktestSection(weekly, events, map[string]Answer{"rework depth": answer(true)}, map[string]Answer{})
	if err != nil {
		t.Fatalf("BacktestSection: %v", err)
	}
	for _, m := range backtestMetrics {
		if !strings.Contains(text, "| "+m.name+" |") {
			t.Errorf("section does not list metric %q", m.name)
		}
	}
	for _, want := range []string{
		"| rework depth |", "insufficient history", "chance of coincidence",
		"weak evidence", "unconfirmed", "2026-10-21",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("section does not contain %q:\n%s", want, text)
		}
	}
}

func TestBacktestReplicatedFromSummaryMapsTargets(t *testing.T) {
	t.Parallel()
	summary := map[string]MangoSummary{
		"rejected_before_landing": {Boxes: 10, Replicating: 3, HoldoutPositives: 158},
		"manual_rescue":           {Boxes: 6, Replicating: 0, HoldoutPositives: 0},
		"reached_overrun_stop":    {Boxes: 0, Replicating: 0, HoldoutPositives: 37},
	}
	got := ReplicatedFromSummary(summary)
	// no holdout positives or no boxes: nothing to say
	if len(got) != 1 || !isTrue(got["rework depth"]) {
		t.Errorf("ReplicatedFromSummary = %v, want only rework depth: true", got)
	}
}

func TestBacktestReplicatedFromSummaryBoxesThatAllFailTheHoldout(t *testing.T) {
	t.Parallel()
	got := ReplicatedFromSummary(map[string]MangoSummary{
		"rejected_before_landing": {Boxes: 4, Replicating: 0, HoldoutPositives: 50},
	})
	if len(got) != 1 || !isFalse(got["rework depth"]) {
		t.Errorf("ReplicatedFromSummary = %v, want only rework depth: false", got)
	}
}

func TestBacktestChanceIsEventsOverComparableChanges(t *testing.T) {
	t.Parallel()
	r := weeklyRows("size_median", 10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1) // 7 comparable changes
	events := []RegimeEvent{{Date: "2026-10-21"}, {Date: "2026-10-01"}}      // both fall in the series
	chance, err := ChanceOfCoincidence(r, "size_median", events, backtestMinChanges)
	if err != nil {
		t.Fatalf("ChanceOfCoincidence: %v", err)
	}
	if chance == nil {
		t.Fatalf("ChanceOfCoincidence = unknown, want 2/7")
	}
	if math.Abs(*chance-2.0/7) > 1e-9 {
		t.Errorf("ChanceOfCoincidence = %v, want %v", *chance, 2.0/7)
	}
}

func TestBacktestChanceNoneWhenTheSeriesIsTooShort(t *testing.T) {
	t.Parallel()
	chance, err := ChanceOfCoincidence(weeklyRows("size_median", 1, 2, 3), "size_median",
		[]RegimeEvent{{Date: "2026-09-22"}}, backtestMinChanges)
	if err != nil {
		t.Fatalf("ChanceOfCoincidence: %v", err)
	}
	if chance != nil {
		t.Errorf("ChanceOfCoincidence = %v, want unknown for a short series", *chance)
	}
}

func TestBacktestSectionWeakEvidenceDoesNotKeep(t *testing.T) {
	t.Parallel()
	weekly := weeklyRows("size_median", 10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1)
	// 3 events / 7 changes > 25%
	events := []RegimeEvent{
		{Date: "2026-10-21", What: "x", Confirmed: true},
		{Date: "2026-10-01", What: "y", Confirmed: true},
		{Date: "2026-09-22", What: "z", Confirmed: true},
	}
	text, err := BacktestSection(weekly, events, map[string]Answer{}, map[string]Answer{"landing size": answer(false)})
	if err != nil {
		t.Fatalf("BacktestSection: %v", err)
	}
	line := sectionLine(t, text, "| landing size |")
	if !strings.Contains(line, "yes (weak)") {
		t.Errorf("landing size line = %q, want it shown weak", line)
	}
	if !strings.HasSuffix(line, "| undecided |") && !strings.HasSuffix(line, "| drop |") {
		t.Errorf("landing size line = %q, want undecided or drop", line)
	}
	if strings.Contains(line, "keep") {
		t.Errorf("landing size line = %q, a luck-explained move must not keep the metric", line)
	}
}

func TestBacktestSectionLowChanceStillKeeps(t *testing.T) {
	t.Parallel()
	weekly := weeklyRows("size_median", 10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1)
	text, err := BacktestSection(weekly, []RegimeEvent{{Date: "2026-10-21", What: "x", Confirmed: true}},
		map[string]Answer{}, map[string]Answer{})
	if err != nil {
		t.Fatalf("BacktestSection: %v", err)
	}
	line := sectionLine(t, text, "| landing size |")
	if !strings.Contains(line, "| yes |") {
		t.Errorf("landing size line = %q, want a plain yes", line)
	}
	if !strings.HasSuffix(line, "| keep |") {
		t.Errorf("landing size line = %q, want keep", line)
	}
}

func TestBacktestReadInputs(t *testing.T) {
	t.Parallel()
	files := map[string][]byte{
		"/docs/regime-events.json": []byte(`[{"date": "2026-10-21", "what": "gate raised", "source": "log", "confirmed": true},
			{"date": "2026-10-01", "what": "host moved"}]`),
		"/docs/sloan-answers.json": []byte(`{"rework depth": true, "rho": false, "handoff size": null}`),
	}
	env := Env{ReadFile: func(path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			return nil, errors.New("no such file")
		}
		return data, nil
	}}
	events, sloan := ReadBacktestInputs(env, "/docs")
	if len(events) != 2 {
		t.Fatalf("events = %v, want 2", events)
	}
	if !events[0].Confirmed || events[1].Confirmed || events[1].What != "host moved" {
		t.Errorf("events = %v, want the file's dates, what and confirmed flags", events)
	}
	if !isTrue(sloan["rework depth"]) || !isFalse(sloan["rho"]) || sloan["handoff size"] != nil {
		t.Errorf("sloan = %v, want true, false and unknown", sloan)
	}
	if _, ok := sloan["mode shift"]; ok {
		t.Errorf("sloan has an answer for a metric the file does not name")
	}
}

func TestBacktestReadInputsMissingOrUnparseableFileIsEmpty(t *testing.T) {
	t.Parallel()
	env := Env{ReadFile: func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "regime-events.json") {
			return []byte("not json"), nil
		}
		return nil, errors.New("no such file")
	}}
	events, sloan := ReadBacktestInputs(env, "/docs")
	if len(events) != 0 || len(sloan) != 0 {
		t.Errorf("ReadBacktestInputs = %v, %v; want nothing", events, sloan)
	}
}
