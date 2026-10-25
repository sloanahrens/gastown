package metrics

import (
	"math"
	"strings"
	"testing"
)

// tolerance is how far a float may be from the Python test's expected value:
// the figures are means and quantiles, so 1e-9 is exact enough to catch a
// changed formula and loose enough for float64 rounding.
const tolerance = 1e-9

// summaryLanding is test_town_metrics_metrics_a.py's L(): a readable landing of
// 10 lines, one attempt, nothing rejected or rescued.
func summaryLanding(bead, week, rig string) Row {
	return Row{
		"bead": bead, "week": week, "rig": rig,
		"lines_total": "10", "file_entropy": "1.0", "package_entropy": "0.0",
		"attempts": "1", "rejected_before_landing": "0", "manual_rescue": "0",
		"unreadable": "0",
	}
}

// unreadableLanding is L(total=None): an unreadable commit has no diff columns
// at all.
func unreadableLanding(bead, week, rig string) Row {
	r := summaryLanding(bead, week, rig)
	r["lines_total"], r["file_entropy"], r["package_entropy"] = "", "", ""
	r["unreadable"] = "1"
	return r
}

func near(got, want float64) bool {
	return math.Abs(got-want) <= tolerance
}

func wantValue(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", name, want)
	}
	if !near(*got, want) {
		t.Errorf("%s = %v, want %v", name, *got, want)
	}
}

func wantNil(t *testing.T, name string, got *float64) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %v, want None", name, *got)
	}
}

func TestPercentileInterpolates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []float64
		p      float64
		want   float64
	}{
		{"the middle of an even count interpolates", []float64{1, 2, 3, 4}, 0.5, 2.5},
		{"one value is itself", []float64{5}, 0.9, 5},
		{"the top of the range interpolates", []float64{0, 10}, 0.9, 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Percentile(tc.values, tc.p); !near(got, tc.want) {
				t.Errorf("Percentile(%v, %v) = %v, want %v", tc.values, tc.p, got, tc.want)
			}
		})
	}
}

func TestWeeklyLandingMetricsSizeAndReworkByWeekRigAndTown(t *testing.T) {
	t.Parallel()
	a := summaryLanding("a", "w1", "gastown")
	b := summaryLanding("b", "w1", "gastown")
	b["lines_total"], b["attempts"], b["rejected_before_landing"] = "30", "3", "1"
	c := summaryLanding("c", "w1", "devops")
	c["lines_total"], c["manual_rescue"] = "20", "1"
	rows := []Row{a, b, c, unreadableLanding("d", "w1", "gastown")}

	out, err := WeeklyLandingMetrics(rows)
	if err != nil {
		t.Fatalf("WeeklyLandingMetrics: %v", err)
	}
	g := out[WeekRig{Week: "w1", Rig: "gastown"}]
	if g.Landings != 3 {
		t.Errorf("gastown landings = %d, want 3", g.Landings)
	}
	if g.Unreadable != 1 {
		t.Errorf("gastown unreadable = %d, want 1", g.Unreadable)
	}
	wantValue(t, "gastown size_median", g.SizeMedian, 20) // readable sizes: 10, 30
	wantValue(t, "gastown attempts_mean", g.AttemptsMean, 5.0/3)
	wantValue(t, "gastown rejected_share", &g.RejectedShare, 1.0/3)
	wantValue(t, "gastown first_try_share", &g.FirstTryShare, 2.0/3)

	town := out[WeekRig{Week: "w1", Rig: "ALL"}]
	if town.Landings != 4 {
		t.Errorf("town landings = %d, want 4", town.Landings)
	}
	if town.ManualRescueCount != 1 {
		t.Errorf("town manual_rescue_count = %d, want 1", town.ManualRescueCount)
	}
	if !town.Thin {
		t.Error("town thin = false, want true below ten landings")
	}
}

func TestWeeklyLandingMetricsNoReadableDiffGivesNoneNotZero(t *testing.T) {
	t.Parallel()
	out, err := WeeklyLandingMetrics([]Row{unreadableLanding("a", "w1", "gastown")})
	if err != nil {
		t.Fatalf("WeeklyLandingMetrics: %v", err)
	}
	g := out[WeekRig{Week: "w1", Rig: "gastown"}]
	wantNil(t, "size_median", g.SizeMedian)
	wantNil(t, "file_entropy_mean", g.FileEntropyMean)
}

func TestWeeklyLandingMetricsTenLandingsIsNotThin(t *testing.T) {
	t.Parallel()
	rows := make([]Row, 0, 10)
	for _, bead := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"} {
		rows = append(rows, summaryLanding(bead, "w1", "gastown"))
	}
	out, err := WeeklyLandingMetrics(rows)
	if err != nil {
		t.Fatalf("WeeklyLandingMetrics: %v", err)
	}
	if town := out[WeekRig{Week: "w1", Rig: "ALL"}]; town.Thin {
		t.Error("town thin = true, want false at ten landings")
	}
}

func TestWeeklyLandingMetricsRejectsANonNumericCell(t *testing.T) {
	t.Parallel()
	row := summaryLanding("a", "w1", "gastown")
	row["attempts"] = "many"
	_, err := WeeklyLandingMetrics([]Row{row})
	if err == nil {
		t.Fatal("a non-numeric attempts cell was accepted; want an error")
	}
	if !strings.Contains(err.Error(), "attempts") || !strings.Contains(err.Error(), "row 2") {
		t.Errorf("error = %q, want the column and the CSV row named", err)
	}
}

func TestClassifyRejection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind string
		want string
	}{
		{"conflict", "conflict"},
		{"gate", "gate"},
		{"review", "review"},
		{"policy", "policy"},
		{"race", "race"},
		{"GATE", "gate"},
		{"mystery", "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyRejection(tc.kind); got != tc.want {
				t.Errorf("ClassifyRejection(%q) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestNonConservative(t *testing.T) {
	t.Parallel()
	out := NonConservative([]string{"conflict", "gate", "review", "unknown"})
	if out.Rejections != 4 {
		t.Errorf("rejections = %d, want 4", out.Rejections)
	}
	wantValue(t, "conflict_gate_share", out.ConflictGateShare, 0.5)
	wantValue(t, "unknown_share", out.UnknownShare, 0.25)

	none := NonConservative(nil)
	if none.Rejections != 0 {
		t.Errorf("no kinds: rejections = %d, want 0", none.Rejections)
	}
	wantNil(t, "no kinds: conflict_gate_share", none.ConflictGateShare)
	wantNil(t, "no kinds: unknown_share", none.UnknownShare)
}
