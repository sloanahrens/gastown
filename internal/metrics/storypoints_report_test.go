package metrics

import (
	"bytes"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
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

// driftRow is one landed bead's drift cells: it landed at landed, its a priori
// estimate is estMinutes, and its measured polecat time is seconds. Its estimate
// was written the day before its first dispatch, so it is an a priori estimate.
func driftRow(bead string, landed time.Time, estMinutes, seconds float64) Row {
	return Row{
		"bead":                bead,
		"landed_at":           landed.UTC().Format(time.RFC3339),
		"sp_minutes":          strconv.FormatFloat(estMinutes, 'f', -1, 64),
		"polecat_secs":        strconv.FormatFloat(seconds, 'f', -1, 64),
		"polecat_lower_bound": "0",
		"sp_estimated_at":     "2026-09-01T10:00:00Z",
		"dispatched_at":       "2026-09-02T10:00:00Z",
	}
}

// driftLandings builds n landed beads, the i-th estimated i+1 minutes, landed one
// minute apart from base, with the measured ranks in the permutation measuredRank
// gives.
func driftLandings(n int, base time.Time, measuredRank func(i int) float64) []Row {
	rows := make([]Row, 0, n)
	for i := range n {
		rows = append(rows, driftRow("gt-d"+strconv.Itoa(i), base.Add(time.Duration(i)*time.Minute),
			float64(i+1), measuredRank(i)*60))
	}
	return rows
}

// TestStoryPointsDriftTable: over 30 landings whose estimate is 1..30 and whose
// measured rank is the estimate with the two ends swapped (rank 1 and rank 30
// change places, the other 28 keep theirs), the rank correlation is the hand
// value. The two moved ranks each move 29 places, so the sum of squared rank
// gaps is 2*29^2 = 1682, and rho = 1 - 6*1682/(30*(30^2-1)) = 16878/26970. The
// measured time is the squared rank, so it ranks with the estimate but is not a
// straight line in it: a correlation of the raw series does not give this value.
func TestStoryPointsDriftTable(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	rows := driftLandings(30, base, func(i int) float64 {
		switch i {
		case 0:
			return 30 * 30
		case 29:
			return 1
		}
		return float64((i + 1) * (i + 1))
	})

	got, err := StoryPointsDriftOver(rows, nil)
	if err != nil {
		t.Fatalf("StoryPointsDriftOver: %v", err)
	}
	if got.N != 30 {
		t.Errorf("window N = %d, want 30", got.N)
	}
	if !got.Current.Measurable {
		t.Fatalf("Current is not measurable: %s", got.Current.Why)
	}
	if want := 16878.0 / 26970.0; math.Abs(got.Current.Rho-want) > 1e-12 {
		t.Errorf("Current.Rho = %.12f, want %.12f (hand-computed from 2*29^2 over 30 landings)", got.Current.Rho, want)
	}
	if got.Current.Why != "" {
		t.Errorf("Current.Why = %q, want empty", got.Current.Why)
	}
}

// TestStoryPointsDriftUsesTheLatestHundred: with 120 eligible landings and rows
// that are not in landing order, the window is the 100 that landed last — the
// oldest 20, whose measured time reverses their estimate, are not read. Five
// ineligible rows land after every eligible one, so a filter that lets one
// through would displace a landing of the window.
func TestStoryPointsDriftUsesTheLatestHundred(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	rows := driftLandings(120, base, func(i int) float64 {
		if i < 20 {
			return float64(120 - i) // the oldest 20 reverse their estimate
		}
		return float64(i + 1) // the newest 100 rank with their estimate
	})
	// The ineligible rows land last, and each one's pair is out of rank order:
	// let through, it would take the place of gt-d20 (estimate 21 minutes, measured
	// 21) and break the perfect ranking, so the correlation and not only the count
	// catches a filter that dropped it.
	newest := base.Add(121 * time.Minute)
	rows = append(rows,
		rowWithout("sp_minutes")(driftRow("gt-no-minutes", newest, 21, 7200)),
		rowWithout("polecat_secs")(driftRow("gt-no-seconds", newest, 21, 7200)),
		rowWith("polecat_lower_bound", "1")(driftRow("gt-lower-bound", newest, 21, 7200)),
		rowWith("sp_estimated_at", "2026-09-03T10:00:00Z")(driftRow("gt-after-dispatch", newest, 21, 7200)),
		// A zero time would rank first while its estimate ranks last, so a filter
		// that let it through would move the correlation.
		rowWith("polecat_secs", "0")(driftRow("gt-zero-seconds", newest, 120, 0)),
	)

	var beads []string
	got, err := StoryPointsDriftOver(rows, func(row Row) (float64, bool) {
		beads = append(beads, row["bead"])
		minutes, _ := strconv.ParseFloat(row["sp_minutes"], 64)
		return minutes, true
	})
	if err != nil {
		t.Fatalf("StoryPointsDriftOver: %v", err)
	}
	if got.N != 100 {
		t.Errorf("window N = %d, want 100", got.N)
	}
	if !got.Current.Measurable || got.Current.Rho != 1 {
		t.Errorf("Current = %+v, want the perfect ranking of the newest 100", got.Current)
	}
	if !got.Previous.Measurable || got.Previous.Rho != 1 {
		t.Errorf("Previous = %+v, want the same landings ranked perfectly", got.Previous)
	}
	if len(beads) != 100 {
		t.Fatalf("previous scored %d landings, want the window's 100", len(beads))
	}
	for i := range 20 {
		old := "gt-d" + strconv.Itoa(i)
		for _, bead := range beads {
			if bead == old {
				t.Errorf("previous weights scored %s, an old landing outside the window", old)
			}
		}
	}
}

// rowWithout is the row with one cell absent.
func rowWithout(column string) func(Row) Row {
	return func(row Row) Row {
		delete(row, column)
		return row
	}
}

// rowWith is the row with one cell set.
func rowWith(column, value string) func(Row) Row {
	return func(row Row) Row {
		row[column] = value
		return row
	}
}

// TestStoryPointsDriftBelowThirtyIsUnmeasurable: 29 landings are too few to read
// and report no number; 30 are read.
func TestStoryPointsDriftBelowThirtyIsUnmeasurable(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	perfect := func(i int) float64 { return float64(i + 1) }

	below, err := StoryPointsDriftOver(driftLandings(29, base, perfect), nil)
	if err != nil {
		t.Fatalf("StoryPointsDriftOver 29: %v", err)
	}
	if below.Current.Measurable || !math.IsNaN(below.Current.Rho) {
		t.Errorf("29 landings = %+v, want unmeasurable and no number", below.Current)
	}
	if want := "29 landings with both an estimate and a measured time, need 30"; below.Current.Why != want {
		t.Errorf("Why = %q, want %q", below.Current.Why, want)
	}

	at, err := StoryPointsDriftOver(driftLandings(30, base, perfect), nil)
	if err != nil {
		t.Fatalf("StoryPointsDriftOver 30: %v", err)
	}
	if !at.Current.Measurable || at.Current.Rho != 1 {
		t.Errorf("30 landings = %+v, want a measurable perfect ranking", at.Current)
	}
}

// TestStoryPointsDriftNoSpreadIsUnmeasurable: a window whose estimates are all
// tied has no rank correlation to report.
func TestStoryPointsDriftNoSpreadIsUnmeasurable(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	rows := driftLandings(30, base, func(i int) float64 { return float64(i+1) + 10 })
	for i := range rows {
		rows[i]["sp_minutes"] = "8"
	}

	got, err := StoryPointsDriftOver(rows, nil)
	if err != nil {
		t.Fatalf("StoryPointsDriftOver: %v", err)
	}
	if got.Current.Measurable || !math.IsNaN(got.Current.Rho) {
		t.Errorf("Current = %+v, want unmeasurable and no number", got.Current)
	}
	if !strings.Contains(got.Current.Why, ErrNoSpread.Error()) {
		t.Errorf("Why = %q, want it to name %v", got.Current.Why, ErrNoSpread)
	}
}

// TestStoryPointsDriftRefusesBadCells: a cell that does not parse stops the
// measure; it is not read as a landing without that value.
func TestStoryPointsDriftRefusesBadCells(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		edit func(Row) Row
	}{
		{name: "sp_minutes not a number", edit: rowWith("sp_minutes", "soon")},
		{name: "sp_minutes zero", edit: rowWith("sp_minutes", "0")},
		{name: "polecat_secs not a number", edit: rowWith("polecat_secs", "soon")},
		{name: "no landed_at", edit: rowWithout("landed_at")},
		{name: "unreadable landed_at", edit: rowWith("landed_at", "soon")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := driftLandings(30, base, func(i int) float64 { return float64(i + 1) })
			rows[0] = tc.edit(rows[0])
			if _, err := StoryPointsDriftOver(rows, nil); err == nil {
				t.Error("StoryPointsDriftOver returned no error")
			}
		})
	}
}

// TestStoryPointsDriftRefusesANonNumberFromThePreviousWeights: a scorer that
// says it scored a landing but returns no positive number is a caller bug, not a
// landing to leave out.
func TestStoryPointsDriftRefusesANonNumberFromThePreviousWeights(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	rows := driftLandings(30, base, func(i int) float64 { return float64(i + 1) })
	for _, minutes := range []float64{math.NaN(), 0, -1, math.Inf(1)} {
		_, err := StoryPointsDriftOver(rows, func(Row) (float64, bool) { return minutes, true })
		if err == nil {
			t.Errorf("previous score %v returned no error", minutes)
		}
	}
}

// TestStoryPointsDriftPreviousReading: the second reading is over the window,
// and it is unmeasurable rather than a number over a smaller sample when the
// previous weights cannot score every landing.
func TestStoryPointsDriftPreviousReading(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	rows := driftLandings(30, base, func(i int) float64 { return float64(i + 1) })

	tests := []struct {
		name       string
		previous   StoryPointsDriftPrevious
		measurable bool
		rho        float64
		why        string
	}{
		{
			name:       "every landing scored",
			previous:   func(row Row) (float64, bool) { m, _ := strconv.ParseFloat(row["sp_minutes"], 64); return 3 * m, true },
			measurable: true,
			rho:        1,
		},
		{
			name: "one landing not scored",
			previous: func(row Row) (float64, bool) {
				return 5, row["bead"] != "gt-d7"
			},
			why: "the previous weights cannot score 1 of 30 landings",
		},
		{
			name: "no scorer",
			why:  "no previous-weights score",
		},
		{
			name:     "every landing scored the same",
			previous: func(Row) (float64, bool) { return 5, true },
			why:      ErrNoSpread.Error(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := StoryPointsDriftOver(rows, tc.previous)
			if err != nil {
				t.Fatalf("StoryPointsDriftOver: %v", err)
			}
			if got.Previous.Measurable != tc.measurable {
				t.Fatalf("Previous = %+v, want measurable %t", got.Previous, tc.measurable)
			}
			if tc.measurable {
				if got.Previous.N != got.N || math.Abs(got.Previous.Rho-tc.rho) > 1e-12 {
					t.Errorf("Previous = %+v, want rho %v over the window's %d landings", got.Previous, tc.rho, got.N)
				}
				return
			}
			if !math.IsNaN(got.Previous.Rho) {
				t.Errorf("Previous.Rho = %v, want no number", got.Previous.Rho)
			}
			if !strings.Contains(got.Previous.Why, tc.why) {
				t.Errorf("Why = %q, want it to name %q", got.Previous.Why, tc.why)
			}
		})
	}
}
