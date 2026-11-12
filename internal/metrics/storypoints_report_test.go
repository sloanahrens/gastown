package metrics

import (
	"bytes"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestSpearmanKnownRankings: a perfect ranking is 1, a reversed one is -1, and
// ties share the average of their ranks.
func TestSpearmanKnownRankings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		x, y []float64
		want float64
	}{
		{name: "perfect ranking", x: []float64{1, 2, 3, 4, 5}, y: []float64{10, 20, 30, 40, 50}, want: 1},
		{name: "reversed ranking", x: []float64{1, 2, 3, 4, 5}, y: []float64{50, 40, 30, 20, 10}, want: -1},
		// Ranks of x are 1, 2.5, 2.5, 4; y is ranked 1..4. The tied pair shares 2.5.
		{name: "ties in one series", x: []float64{1, 2, 2, 3}, y: []float64{1, 2, 3, 4}, want: 3 / math.Sqrt(10)},
		{name: "monotone with a tie in both", x: []float64{1, 1, 2, 3}, y: []float64{5, 5, 6, 9}, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Spearman(tc.x, tc.y)
			if err != nil {
				t.Fatalf("Spearman: %v", err)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("Spearman = %.12f, want %.12f", got, tc.want)
			}
		})
	}
}

// TestSpearmanRefusesWhatHasNoAnswer: a length mismatch, fewer than two pairs,
// and a series with no spread are errors, not a correlation of 0.
func TestSpearmanRefusesWhatHasNoAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		x, y     []float64
		noSpread bool
	}{
		{name: "length mismatch", x: []float64{1, 2}, y: []float64{1}},
		{name: "one pair", x: []float64{1}, y: []float64{1}},
		{name: "estimate all tied", x: []float64{3, 3, 3}, y: []float64{1, 2, 3}, noSpread: true},
		{name: "measure all tied", x: []float64{1, 2, 3}, y: []float64{8, 8, 8}, noSpread: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Spearman(tc.x, tc.y)
			if err == nil {
				t.Fatal("Spearman returned no error")
			}
			if tc.noSpread && !errors.Is(err, ErrNoSpread) {
				t.Errorf("err = %v, want ErrNoSpread", err)
			}
		})
	}
}

// TestBootstrapSpearmanIsTheSameOnEveryRun: the interval is a function of the
// pairs and the seed, so two runs print one interval; a perfect ranking has no
// resample that breaks it, so its interval is exactly 1.
func TestBootstrapSpearmanIsTheSameOnEveryRun(t *testing.T) {
	t.Parallel()
	x := []float64{1, 2, 3, 5, 8, 13, 21, 2, 3, 5, 8, 1}
	y := []float64{2, 1, 4, 3, 9, 13, 30, 5, 2, 6, 7, 1}
	lo1, hi1, err := BootstrapSpearman(x, y, bootstrapResamples, bootstrapSeed)
	if err != nil {
		t.Fatalf("BootstrapSpearman: %v", err)
	}
	lo2, hi2, err := BootstrapSpearman(x, y, bootstrapResamples, bootstrapSeed)
	if err != nil {
		t.Fatalf("BootstrapSpearman second run: %v", err)
	}
	if lo1 != lo2 || hi1 != hi2 {
		t.Errorf("runs differ: [%v, %v] then [%v, %v]", lo1, hi1, lo2, hi2)
	}
	if !(lo1 <= hi1 && lo1 >= -1 && hi1 <= 1) {
		t.Errorf("interval [%v, %v] is not an ordered interval in [-1, 1]", lo1, hi1)
	}

	perfect := []float64{1, 2, 3, 4, 5, 6}
	lo, hi, err := BootstrapSpearman(perfect, perfect, bootstrapResamples, bootstrapSeed)
	if err != nil {
		t.Fatalf("BootstrapSpearman perfect: %v", err)
	}
	if math.Abs(lo-1) > 1e-9 || math.Abs(hi-1) > 1e-9 {
		t.Errorf("perfect ranking interval = [%v, %v], want [1, 1]", lo, hi)
	}
}

// TestBootstrapSpearmanRefusesAllTiedResamples: when no resample has spread,
// there is no interval to print.
func TestBootstrapSpearmanRefusesAllTiedResamples(t *testing.T) {
	t.Parallel()
	_, _, err := BootstrapSpearman([]float64{4, 4}, []float64{1, 2}, bootstrapResamples, bootstrapSeed)
	if !errors.Is(err, ErrNoSpread) {
		t.Errorf("err = %v, want ErrNoSpread", err)
	}
}

// TestTriggerTableFlagsPrecisionAndRecall: on a small synthetic table each
// trigger's flagged count, precision and recall are the hand-counted values. A
// bead ran long when its measured points are over 21.
func TestTriggerTableFlagsPrecisionAndRecall(t *testing.T) {
	t.Parallel()
	est := []float64{12, 14, 16, 18, 20, 9}
	actual := []int{5, 34, 55, 8, 21, 34}
	// Long beads: indexes 1, 2 and 5, so three in all.
	rows, err := TriggerTable(est, actual, []float64{11, 13, 15, 17, 25})
	if err != nil {
		t.Fatalf("TriggerTable: %v", err)
	}
	tests := []struct {
		minutes      float64
		flagged, hit int
		precision    float64
		precisionOK  bool
		recall       float64
	}{
		{minutes: 11, flagged: 5, hit: 2, precision: 0.4, precisionOK: true, recall: 2.0 / 3},
		{minutes: 13, flagged: 4, hit: 2, precision: 0.5, precisionOK: true, recall: 2.0 / 3},
		{minutes: 15, flagged: 3, hit: 1, precision: 1.0 / 3, precisionOK: true, recall: 1.0 / 3},
		{minutes: 17, flagged: 2, hit: 0, precision: 0, precisionOK: true, recall: 0},
		{minutes: 25, flagged: 0, hit: 0, precisionOK: false, recall: 0},
	}
	if len(rows) != len(tests) {
		t.Fatalf("rows = %d, want %d", len(rows), len(tests))
	}
	for i, tc := range tests {
		r := rows[i]
		if r.Minutes != tc.minutes || r.Flagged != tc.flagged || r.Caught != tc.hit || r.Long != 3 {
			t.Errorf("trigger %v = %+v, want flagged %d caught %d long 3", tc.minutes, r, tc.flagged, tc.hit)
		}
		p, pok := r.Precision()
		if pok != tc.precisionOK || (pok && math.Abs(p-tc.precision) > 1e-9) {
			t.Errorf("trigger %v precision = %v (%t), want %v (%t)", tc.minutes, p, pok, tc.precision, tc.precisionOK)
		}
		rc, rok := r.Recall()
		if !rok || math.Abs(rc-tc.recall) > 1e-9 {
			t.Errorf("trigger %v recall = %v (%t), want %v", tc.minutes, rc, rok, tc.recall)
		}
	}
}

// TestTriggerTableRefusesMismatchedColumns: an estimate with no measure is not
// a row to count.
func TestTriggerTableRefusesMismatchedColumns(t *testing.T) {
	t.Parallel()
	if _, err := TriggerTable([]float64{12, 14}, []int{5}, []float64{11}); err == nil {
		t.Error("TriggerTable returned no error for two estimates and one measure")
	}
}

// spLandingRow is one landing row with the story-point cells the report reads.
// Its estimate is written the day before its first dispatch, so the row is scored.
func spLandingRow(i int, est, actual string, seat, rung string) Row {
	return Row{
		"bead": "gt-" + strconv.Itoa(i), "sp": est, "sp_actual": actual, "seat_class": seat, "sp_rung": rung,
		"sp_estimated_at": "2026-10-19T10:00:00Z", "dispatched_at": "2026-10-20T10:00:00Z",
	}
}

// measuredRows builds n landed beads with an estimate and a measured time, across
// two seat classes and two rungs, with spread in both series.
func measuredRows(n int) []Row {
	points := []int{1, 2, 3, 5, 8, 13, 21, 34}
	rows := make([]Row, 0, n)
	for i := range n {
		est := points[i%len(points)]
		actual := points[(i*3+1)%len(points)]
		seat := "haiku"
		if i%2 == 1 {
			seat = "flash"
		}
		rung := "all"
		if i%3 == 0 {
			rung = "shape-jev"
		}
		rows = append(rows, spLandingRow(i, strconv.Itoa(est), strconv.Itoa(actual), seat, rung))
	}
	return rows
}

// TestShadowReportBelowSixtyPrintsTheCountOnly: with 59 estimated, measured
// landings the report prints the count and nothing else.
func TestShadowReportBelowSixtyPrintsTheCountOnly(t *testing.T) {
	t.Parallel()
	rows := append(measuredRows(59), spLandingRow(99, "", "", "", "none"), spLandingRow(98, "8", "", "haiku", "all"))
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, rows); err != nil {
		t.Fatalf("WriteStoryPointsReport: %v", err)
	}
	want := "story points: 59 estimated landings with a measured time; the report needs 60\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// TestShadowReportAtSixtyPrintsTheTables: with 60 estimated, measured landings
// the report prints the per-class and per-rung tables, the unmeasured and
// rung-none counts, and the trigger line.
func TestShadowReportAtSixtyPrintsTheTables(t *testing.T) {
	t.Parallel()
	rows := append(measuredRows(60), spLandingRow(98, "8", "", "haiku", "all"), spLandingRow(99, "3", "", "", "none"))
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, rows); err != nil {
		t.Fatalf("WriteStoryPointsReport: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"story points shadow report: 60 estimated landings with a measured time",
		"per seat class\n",
		"haiku",
		"flash",
		"per rung\n",
		"shape-jev",
		"estimated but unmeasured: 2; rung none: 1",
		"estimates written after dispatch: 0",
		"split trigger\n  no estimated minutes recorded yet\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}

// TestShadowReportPrintsTheTriggerTableWhenMinutesAreThere: when every landed bead
// carries its estimated minutes the report prints the four trigger rows.
func TestShadowReportPrintsTheTriggerTableWhenMinutesAreThere(t *testing.T) {
	t.Parallel()
	rows := measuredRows(60)
	for i := range rows {
		rows[i]["sp_minutes"] = strconv.Itoa(8 + i%12)
	}
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, rows); err != nil {
		t.Fatalf("WriteStoryPointsReport: %v", err)
	}
	got := out.String()
	for _, minutes := range []string{"  11 min", "  13 min", "  15 min", "  17 min"} {
		if !strings.Contains(got, minutes) {
			t.Errorf("report lacks trigger row %q:\n%s", minutes, got)
		}
	}
	if strings.Contains(got, "not computed") {
		t.Errorf("report says the trigger is not computed although every bead carries minutes:\n%s", got)
	}
}

// TestShadowReportRefusesAnUnreadableCell: a points cell that is not a point on
// the scale stops the report; it is not counted as a landing.
func TestShadowReportRefusesAnUnreadableCell(t *testing.T) {
	t.Parallel()
	rows := append(measuredRows(60), spLandingRow(97, "4", "5", "haiku", "all"))
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, rows); err == nil {
		t.Errorf("WriteStoryPointsReport returned no error for sp 4; output:\n%s", out.String())
	}
}

// TestShadowReportLeavesOutAnEstimateWithoutAUsableWriteBeforeDispatch: an
// estimate written at or after the bead's first dispatch, or with a missing or
// unreadable time on either side, is not scored. With 59 scored beads the report
// stays at the count; with 60 it prints the tables and counts the excluded row.
func TestShadowReportLeavesOutAnEstimateWithoutAUsableWriteBeforeDispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(Row)
	}{
		{name: "written after dispatch", edit: func(r Row) { r["sp_estimated_at"] = "2026-10-21T10:00:00Z" }},
		{name: "written at dispatch", edit: func(r Row) { r["sp_estimated_at"] = "2026-10-20T10:00:00Z" }},
		{name: "no write time", edit: func(r Row) { delete(r, "sp_estimated_at") }},
		{name: "unreadable write time", edit: func(r Row) { r["sp_estimated_at"] = "soon" }},
		{name: "no dispatch time", edit: func(r Row) { r["dispatched_at"] = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			excluded := spLandingRow(95, "5", "5", "haiku", "all")
			tc.edit(excluded)

			var below bytes.Buffer
			if err := WriteStoryPointsReport(&below, append(measuredRows(59), excluded)); err != nil {
				t.Fatalf("WriteStoryPointsReport: %v", err)
			}
			want := "story points: 59 estimated landings with a measured time; the report needs 60\n"
			if below.String() != want {
				t.Errorf("below sixty output = %q, want %q", below.String(), want)
			}

			var at bytes.Buffer
			if err := WriteStoryPointsReport(&at, append(measuredRows(60), excluded)); err != nil {
				t.Fatalf("WriteStoryPointsReport at sixty: %v", err)
			}
			if got := at.String(); !strings.Contains(got, "shadow report: 60 estimated landings") ||
				!strings.Contains(got, "estimates written after dispatch: 1\n") {
				t.Errorf("at sixty output lacks the scored count or the excluded line:\n%s", got)
			}
		})
	}
}

// TestShadowReportSaysNoMinutesYetWhenNoLandingHasThem: with no sp_minutes cell on
// any landing the trigger section says so on its own line.
func TestShadowReportSaysNoMinutesYetWhenNoLandingHasThem(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, measuredRows(60)); err != nil {
		t.Fatalf("WriteStoryPointsReport: %v", err)
	}
	if !strings.Contains(out.String(), "split trigger\n  no estimated minutes recorded yet\n") {
		t.Errorf("report lacks the no-minutes line in the trigger section:\n%s", out.String())
	}
}

// TestShadowReportNamesTheBeadsWithoutMinutesWhenSomeHaveThem: with minutes on
// some landings and not all, the trigger table is not written over the rest; the
// section counts the landings that carry none.
func TestShadowReportNamesTheBeadsWithoutMinutesWhenSomeHaveThem(t *testing.T) {
	t.Parallel()
	rows := measuredRows(60)
	for i := range rows[:59] {
		rows[i]["sp_minutes"] = strconv.Itoa(8 + i%12)
	}
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, rows); err != nil {
		t.Fatalf("WriteStoryPointsReport: %v", err)
	}
	want := "split trigger: not computed, 1 of 60 landings carry no estimated minutes\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("report lacks %q:\n%s", want, out.String())
	}
}
