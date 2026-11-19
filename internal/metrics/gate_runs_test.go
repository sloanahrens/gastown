package metrics

import (
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// gateRunHeader is the column list gate_runs.csv is written under, in order.
var gateRunHeader = []string{"ts", "rig", "bead", "sha", "run_id", "queue_s", "run_s", "setup_s", "lint_s", "build_s", "unit_wall_s", "tier_s", "tier_bar_s", "over", "pole_pkg", "pole_s", "num_cpu", "gomaxprocs", "cgroup", "cached_pkgs", "passed", "running_runs", "runner_periods", "runner_throttled", "runner_throttled_usec"}

// gateRunFixture is a run with every field off its zero value.
func gateRunFixture() land.GateRun {
	return land.GateRun{
		TS: time.Date(2026, 11, 19, 4, 19, 5, 0, time.UTC), Rig: "gastown", Bead: "gt-qh6p8w", SHA: "4436f7b1", RunID: "run-1",
		QueueS: 12.5, RunS: 139, SetupS: 2, LintS: 3.25, BuildS: 20, UnitWallS: 110, TierS: 60, TierBarS: 45,
		Over: true, PolePkg: "internal/land", PoleS: 40,
		NumCPU: 24, GOMAXPROCS: 8, Cgroup: "/gt/polecats/dementus", CachedPkgs: 3, Passed: true,
		RunningRuns: 2, RunnerPeriods: 100, RunnerThrottled: 7, RunnerThrottledUsec: 900,
	}
}

// gateRunByName maps one row's cells to their header names.
func gateRunByName(t *testing.T, columns, row []string) map[string]string {
	t.Helper()
	if len(row) != len(columns) {
		t.Fatalf("row has %d cells, header has %d", len(row), len(columns))
	}
	cells := make(map[string]string, len(columns))
	for i, name := range columns {
		cells[name] = row[i]
	}
	return cells
}

func TestGateRunRowsHeaderAndOneRowPerRunWithBooleansAsOneOrZero(t *testing.T) {
	t.Parallel()
	failed := gateRunFixture()
	failed.Bead = "gt-other"
	failed.Over = false
	failed.Passed = false
	columns, rows := GateRunRows([]land.GateRun{gateRunFixture(), failed})
	if !slices.Equal(columns, gateRunHeader) {
		t.Errorf("header = %v, want %v", columns, gateRunHeader)
	}
	if len(rows) != 2 {
		t.Fatalf("two runs gave %d rows, want 2", len(rows))
	}
	passed := gateRunByName(t, columns, rows[0])
	if passed["over"] != "1" || passed["passed"] != "1" {
		t.Errorf("passing run over/passed = %q/%q, want 1/1", passed["over"], passed["passed"])
	}
	failedCells := gateRunByName(t, columns, rows[1])
	if failedCells["over"] != "0" || failedCells["passed"] != "0" || failedCells["bead"] != "gt-other" {
		t.Errorf("failing run over/passed/bead = %q/%q/%q, want 0/0/gt-other", failedCells["over"], failedCells["passed"], failedCells["bead"])
	}
}

func TestGateRunRowsCellFormats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		edit   func(*land.GateRun)
		column string
		want   string
	}{
		{"a time is written in UTC", func(r *land.GateRun) { r.TS = time.Date(2026, 11, 19, 6, 19, 5, 0, time.FixedZone("UTC+2", 7200)) }, "ts", "2026-11-19T04:19:05Z"},
		{"an unset time is an empty cell", func(r *land.GateRun) { r.TS = time.Time{} }, "ts", ""},
		{"empty text is an empty cell", func(r *land.GateRun) { r.PolePkg = "" }, "pole_pkg", ""},
		{"a fractional seconds value keeps its digits", func(*land.GateRun) {}, "lint_s", "3.25"},
		{"a whole seconds value keeps its .0", func(*land.GateRun) {}, "setup_s", "2.0"},
		{"a zero stage time is 0.0", func(r *land.GateRun) { r.SetupS = 0 }, "setup_s", "0.0"},
		{"a zero counter is 0", func(r *land.GateRun) { r.RunnerThrottled = 0 }, "runner_throttled", "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := gateRunFixture()
			tc.edit(&run)
			columns, rows := GateRunRows([]land.GateRun{run})
			if got := gateRunByName(t, columns, rows[0])[tc.column]; got != tc.want {
				t.Errorf("%s = %q, want %q", tc.column, got, tc.want)
			}
		})
	}
}
