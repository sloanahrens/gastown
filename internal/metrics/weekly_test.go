package metrics

import (
	"strconv"
	"testing"
)

// floatPtr returns v's address, for one week of a ModeShift series.
func floatPtr(v float64) *float64 {
	return &v
}

// shiftWeek builds one week of ModeShift's series from the columns it carries.
func shiftWeek(values map[string]float64) map[string]*float64 {
	row := make(map[string]*float64, len(values))
	for column, v := range values {
		row[column] = floatPtr(v)
	}
	return row
}

func TestLabelEntropy(t *testing.T) {
	t.Parallel()
	beads := []Bead{
		{Rig: "gt", Type: "task", Labels: []string{"a"}},
		{Rig: "dv", Type: "task", Labels: []string{"b"}},
	}
	// "b" is rare, so it folds into "other": labels are {a: 1, other: 1} -> 1 bit.
	out := LabelEntropy(beads, map[string]bool{"b": true})
	wantValue(t, "h_labels", &out.HLabels, 1)
	wantValue(t, "h_rigs", &out.HRigs, 1)
	wantValue(t, "h_types", &out.HTypes, 0)
}

func TestDependencyDensity(t *testing.T) {
	t.Parallel()
	got := DependencyDensity([]Bead{{DependencyCount: 2}, {DependencyCount: 4}})
	wantValue(t, "dependency_density", got, 3)
	wantNil(t, "dependency_density of no closed beads", DependencyDensity(nil))
}

func TestRho(t *testing.T) {
	t.Parallel()
	landings := []Row{{"route": "daemon"}, {"route": "daemon"}, {"route": "overseer-manual"}}
	out := Rho(landings, 1)
	if out.Numerator != 2 || out.Denominator != 2 {
		t.Errorf("rho numerator, denominator = %d, %d, want 2, 2", out.Numerator, out.Denominator)
	}
	wantValue(t, "rho", out.Rho, 1)

	none := Rho([]Row{{"route": "overseer-manual"}}, 0)
	if none.Numerator != 1 || none.Denominator != 0 {
		t.Errorf("no daemon landing: numerator, denominator = %d, %d, want 1, 0", none.Numerator, none.Denominator)
	}
	wantNil(t, "rho with no daemon landing", none.Rho)
}

func TestHandoffSize(t *testing.T) {
	t.Parallel()
	out := HandoffSize([]string{"a", "b", "c"}, map[string]int{"a": 100, "b": 300}, map[string]int{"a": 1, "b": 3})
	if out.HandoffBytesTotal != 400 {
		t.Errorf("handoff_bytes_total = %d, want 400", out.HandoffBytesTotal)
	}
	wantValue(t, "handoff_bytes_median", out.HandoffBytesMedian, 200)
	wantValue(t, "sessions_median", out.SessionsMedian, 2)

	empty := HandoffSize(nil, nil, nil)
	if empty.HandoffBytesTotal != 0 {
		t.Errorf("no ids: handoff_bytes_total = %d, want 0", empty.HandoffBytesTotal)
	}
	wantNil(t, "no ids: handoff_bytes_median", empty.HandoffBytesMedian)
}

func TestModeShiftBetweenConsecutiveWeeks(t *testing.T) {
	t.Parallel()
	series := []map[string]*float64{
		shiftWeek(map[string]float64{"x": 0, "y": 0}),
		shiftWeek(map[string]float64{"x": 1, "y": 0}),
		shiftWeek(map[string]float64{"x": 1, "y": 0}),
	}
	out, err := ModeShift(series, []bool{false, false, false}, nil)
	if err != nil {
		t.Fatalf("ModeShift: %v", err)
	}
	if out[0] != nil {
		t.Errorf("first week shift = %v, want None", *out[0])
	}
	if out[1] == nil || *out[1] <= 0 {
		t.Errorf("0 -> 1 shift = %v, want a positive distance", out[1])
	}
	if out[2] == nil || *out[2] != 0 {
		t.Errorf("1 -> 1 shift = %v, want 0", out[2])
	}
}

func TestModeShiftThinWeeksDoNotInflateTheScale(t *testing.T) {
	t.Parallel()
	// The noisy thin week (x=100) must not widen sd: the 0 -> 1 step stays
	// large relative to the non-thin spread.
	series := []map[string]*float64{
		shiftWeek(map[string]float64{"x": 0}),
		shiftWeek(map[string]float64{"x": 1}),
		shiftWeek(map[string]float64{"x": 100}),
		shiftWeek(map[string]float64{"x": 0}),
		shiftWeek(map[string]float64{"x": 1}),
	}
	withThin, err := ModeShift(series, []bool{false, false, true, false, false}, nil)
	if err != nil {
		t.Fatalf("ModeShift with a thin week: %v", err)
	}
	without, err := ModeShift(series[:2], []bool{false, false}, nil)
	if err != nil {
		t.Fatalf("ModeShift without the thin week: %v", err)
	}
	if withThin[1] == nil || without[1] == nil {
		t.Fatalf("0 -> 1 shift = %v with a thin week, %v without one; want both", withThin[1], without[1])
	}
	if !near(*withThin[1], *without[1]) {
		t.Errorf("0 -> 1 shift with a thin week = %v, want %v", *withThin[1], *without[1])
	}
}

func TestModeShiftGapInWeeksGivesNone(t *testing.T) {
	t.Parallel()
	series := []map[string]*float64{
		shiftWeek(map[string]float64{"x": 0}),
		shiftWeek(map[string]float64{"x": 5}),
		shiftWeek(map[string]float64{"x": 6}),
	}
	out, err := ModeShift(series, []bool{false, false, false},
		[]string{"2026-09-07", "2026-09-14", "2026-10-05"})
	if err != nil {
		t.Fatalf("ModeShift: %v", err)
	}
	if out[1] == nil {
		t.Error("consecutive weeks: shift = None, want a distance")
	}
	if out[2] != nil {
		t.Errorf("three weeks apart: shift = %v, want None", *out[2])
	}
}

func TestModeShiftThinOrMissingWeekGivesNone(t *testing.T) {
	t.Parallel()
	series := []map[string]*float64{
		shiftWeek(map[string]float64{"x": 0}),
		shiftWeek(map[string]float64{"x": 5}),
		shiftWeek(nil),
	}
	out, err := ModeShift(series, []bool{false, true, false}, nil)
	if err != nil {
		t.Fatalf("ModeShift: %v", err)
	}
	for i, s := range out {
		if s != nil {
			t.Errorf("week %d shift = %v, want None", i, *s)
		}
	}
}

// weeklyLanding is test_town_metrics_weekly.py's LAND: a readable landing of
// total lines, one attempt, nothing rejected, landed by the daemon.
func weeklyLanding(bead, week, rig string, total int) Row {
	return Row{
		"bead": bead, "week": week, "rig": rig,
		"lines_total": strconv.Itoa(total), "file_entropy": "1.0", "package_entropy": "0.0",
		"attempts": "1", "rejected_before_landing": "0", "manual_rescue": "0",
		"unreadable": "0", "route": "daemon", "om_threshold": "0.7", "dominant_model": "m1",
	}
}

// buildWeeklyRows is the Python test's fixture: two landings and two beads in
// the week of 2026-10-12, one bead closed with a handoff and one rejection kind.
func buildWeeklyRows(t *testing.T) []WeeklyMetrics {
	t.Helper()
	landings := []Row{
		weeklyLanding("a", "2026-10-12", "gastown", 10),
		weeklyLanding("b", "2026-10-12", "devops", 20),
	}
	landings[1]["attempts"], landings[1]["rejected_before_landing"] = "2", "1"
	landings[1]["route"], landings[1]["manual_rescue"] = "overseer-manual", "1"
	landings[1]["dominant_model"] = "m2"
	beads := []Bead{
		{ID: "gt-1", Rig: "gastown", CreatedAt: "2026-10-13T00:00:00Z", ClosedAt: "2026-10-14T00:00:00Z",
			Type: "task", Labels: []string{"handoff"}, DependencyCount: 2},
		{ID: "gt-2", Rig: "gastown", CreatedAt: "2026-10-13T00:00:00Z", Type: "bug"},
	}
	rows, err := BuildWeekly(landings, beads,
		map[string]int{"gt-1": 500}, map[string]int{"gt-1": 3},
		map[WeekRig][]string{{Week: "2026-10-12", Rig: "ALL"}: {"conflict"}},
		map[string]bool{})
	if err != nil {
		t.Fatalf("BuildWeekly: %v", err)
	}
	return rows
}

func weeklyRow(t *testing.T, rows []WeeklyMetrics, week, rig string) WeeklyMetrics {
	t.Helper()
	for _, r := range rows {
		if r.Week == week && r.Rig == rig {
			return r
		}
	}
	t.Fatalf("no row for (%s, %s) in %v", week, rig, rows)
	return WeeklyMetrics{}
}

func TestBuildWeeklyRowsCarryEveryColumn(t *testing.T) {
	t.Parallel()
	rows := buildWeeklyRows(t)

	got := make(map[WeekRig]bool, len(rows))
	for _, r := range rows {
		got[WeekRig{Week: r.Week, Rig: r.Rig}] = true
	}
	for _, key := range []WeekRig{
		{Week: "2026-10-12", Rig: "gastown"},
		{Week: "2026-10-12", Rig: "devops"},
		{Week: "2026-10-12", Rig: "ALL"},
	} {
		if !got[key] {
			t.Errorf("no row for %v; got %v", key, got)
		}
	}
	for _, r := range rows {
		for _, column := range WeeklyColumns {
			if _, ok := r.Cell(column); !ok {
				t.Errorf("(%s, %s): no cell for column %q", r.Week, r.Rig, column)
			}
		}
	}
}

func TestBuildWeeklyTownRowValues(t *testing.T) {
	t.Parallel()
	town := weeklyRow(t, buildWeeklyRows(t), "2026-10-12", "ALL")

	if town.RhoNumerator != 2 { // 1 non-daemon landing + 1 handoff bead closed
		t.Errorf("rho_numerator = %d, want 2", town.RhoNumerator)
	}
	if town.RhoDenominator != 1 {
		t.Errorf("rho_denominator = %d, want 1", town.RhoDenominator)
	}
	wantValue(t, "rho", town.Rho, 2)
	if town.DominantModel == nil || *town.DominantModel != "m1" { // tie broken alphabetically
		t.Errorf("dominant_model = %v, want m1", town.DominantModel)
	}
	wantValue(t, "om_threshold", town.OMThreshold, 0.7)
	if town.HandoffBytesTotal != 500 {
		t.Errorf("handoff_bytes_total = %d, want 500", town.HandoffBytesTotal)
	}
	wantValue(t, "dependency_density", town.DependencyDensity, 2) // only gt-1 closed that week
	wantValue(t, "conflict_gate_share", town.ConflictGateShare, 1)
	wantValue(t, "h_types", town.HTypes, 1) // task and bug created that week

	if cell, _ := town.Cell("rho"); cell != "2.0" {
		t.Errorf("rho cell = %q, want 2.0", cell)
	}
	if cell, _ := town.Cell("thin"); cell != "True" {
		t.Errorf("thin cell = %q, want True", cell)
	}
}

func TestBuildWeeklySortedByRigThenWeek(t *testing.T) {
	t.Parallel()
	rows := buildWeeklyRows(t)
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if a.Rig > b.Rig || (a.Rig == b.Rig && a.Week > b.Week) {
			t.Errorf("row %d (%s, %s) sorts after row %d (%s, %s)", i-1, a.Rig, a.Week, i, b.Rig, b.Week)
		}
	}
	for _, r := range rows {
		if r.ModeShift != nil {
			t.Errorf("(%s, %s): mode_shift = %v, want None for one week", r.Rig, r.Week, *r.ModeShift)
		}
	}
}

func TestBuildWeeklyWeekWithBeadsButNoLandingsStillGetsARow(t *testing.T) {
	t.Parallel()
	beads := []Bead{{ID: "gt-9", Rig: "gastown", CreatedAt: "2026-10-20T00:00:00Z", Type: "task", Labels: []string{"x"}}}
	rows, err := BuildWeekly(nil, beads, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildWeekly: %v", err)
	}
	town := weeklyRow(t, rows, "2026-10-19", "ALL")
	if !town.Thin {
		t.Error("thin = false, want true for a week with no landings")
	}
	if town.Landings != 0 {
		t.Errorf("landings = %d, want 0", town.Landings)
	}
	wantNil(t, "size_median", town.SizeMedian)
	wantNil(t, "rejected_share", town.RejectedShare)
}
