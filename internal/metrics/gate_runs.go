package metrics

import (
	"slices"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// gateRunColumns is the header gate_runs.csv is written under, in order.
var gateRunColumns = []string{"ts", "rig", "bead", "sha", "run_id", "queue_s", "run_s", "setup_s", "lint_s", "build_s", "unit_wall_s", "tier_s", "tier_bar_s", "over", "pole_pkg", "pole_s", "num_cpu", "gomaxprocs", "cgroup", "cached_pkgs", "passed", "running_runs", "runner_periods", "runner_throttled", "runner_throttled_usec"}

// GateRunRows renders one row per gate run in landings.csv's cell format: a
// boolean as 1 or 0, a float through FormatFloat, an empty string as empty.
func GateRunRows(runs []land.GateRun) (columns []string, rows [][]string) {
	rows = make([][]string, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, gateRunCells(run))
	}
	return slices.Clone(gateRunColumns), rows
}

// gateRunCells is one run's cells in gateRunColumns order.
func gateRunCells(run land.GateRun) []string {
	return []string{
		gateRunTS(run.TS),
		run.Rig,
		run.Bead,
		run.SHA,
		run.RunID,
		FormatFloat(run.QueueS),
		FormatFloat(run.RunS),
		FormatFloat(run.SetupS),
		FormatFloat(run.LintS),
		FormatFloat(run.BuildS),
		FormatFloat(run.UnitWallS),
		FormatFloat(run.TierS),
		FormatFloat(run.TierBarS),
		boolCell(run.Over),
		run.PolePkg,
		FormatFloat(run.PoleS),
		strconv.Itoa(run.NumCPU),
		strconv.Itoa(run.GOMAXPROCS),
		run.Cgroup,
		strconv.Itoa(run.CachedPkgs),
		boolCell(run.Passed),
		strconv.Itoa(run.RunningRuns),
		strconv.FormatInt(run.RunnerPeriods, 10),
		strconv.FormatInt(run.RunnerThrottled, 10),
		strconv.FormatInt(run.RunnerThrottledUsec, 10),
	}
}

// gateRunTS is a run's time in UTC RFC3339; an unset (zero) time is empty.
func gateRunTS(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}
