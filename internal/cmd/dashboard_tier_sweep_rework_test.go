package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
)

// A registry that cannot be read is a flag on the trends, not a shorter list:
// a rig missing from the trends may be one the registry lost, not one that has
// not swept. The log's own rigs still draw.
func TestTierSweepTrendsRegistryFailureIsFlagged(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t,
		"2026/10/03 20:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell)",
		"2026/10/03 20:02:00 tier_sweep: gastown: swept a9be03e4 (shell GREEN) in 2m0s",
	)
	r.rigs = func() ([]string, error) { return nil, errors.New("registry unreadable") }
	ts := r.read(time.Date(2026, 10, 3, 23, 0, 0, 0, time.Local))
	if !ts.TrendsUnavailable {
		t.Fatal("a failed registry read must flag the trends as unavailable")
	}
	if len(ts.Trends) != 1 || ts.Trends[0].Rig != "gastown" {
		t.Errorf("trends = %+v, want the log's rig still drawn", ts.Trends)
	}

	healthy, _ := tierSweepTestReader(t)
	if ts := healthy.read(time.Date(2026, 10, 3, 23, 0, 0, 0, time.Local)); ts.TrendsUnavailable {
		t.Error("a readable registry must not flag the trends")
	}
}

// A cycle that aborts after its stage lines leaves nothing behind: the next
// cycle's sweep carries only its own tier walls.
func TestTierSweepAbortedCycleLeavesNoWalls(t *testing.T) {
	t.Parallel()
	aborted := daemon.TierSweepAbortedLine("gastown", "a9be03e4ffff", "no stage ran")
	r, _ := tierSweepTestReader(t,
		"2026/10/03 20:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell, integration)",
		"2026/10/03 20:01:05 tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:",
		"2026/10/03 20:03:00 tier_sweep: gastown: integration finished (exit 0) in 1m55s; last lines:",
		"2026/10/03 20:04:00 "+aborted,
		"2026/10/03 21:00:00 tier_sweep: gastown: sweep started b4c5d6e7 (shell)",
		"2026/10/03 21:00:30 tier_sweep: gastown: shell finished (exit 0) in 30s; last lines:",
		"2026/10/03 21:01:00 tier_sweep: gastown: swept b4c5d6e7 (shell GREEN) in 1m0s",
	)
	ts := r.read(time.Date(2026, 10, 3, 23, 0, 0, 0, time.Local))
	if len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v, want only the swept cycle (the aborted one closes with no row)", ts.Sweeps)
	}
	walls := ts.Trends[0].Sweeps[0].Walls
	if walls["shell"] != 30 || len(walls) != 1 {
		t.Errorf("walls = %v, want only the swept cycle's shell 30s (the aborted cycle's shell and integration must not leak)", walls)
	}
}

// A kept sweep owns its walls: a map the scan keeps writing to is copied, so no
// later write can change a sweep already read.
func TestCopyWallsIsIndependent(t *testing.T) {
	t.Parallel()
	src := map[string]float64{"shell": 65}
	dup := copyWalls(src)
	src["shell"] = 1
	src["race"] = 2
	if dup["shell"] != 65 || len(dup) != 1 {
		t.Errorf("copy = %v, want the walls as they were", dup)
	}
	if copyWalls(nil) != nil {
		t.Error("copy of nil walls must stay nil, so a row omits them")
	}
}

// A cycle replaced by a newer start, with no abort line (the log lost it), starts
// its walls from nothing: the replaced cycle's tiers must not chart in the new one.
func TestTierSweepReplacedCycleStartsItsWallsFromNothing(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t,
		"2026/10/03 20:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell, integration)",
		"2026/10/03 20:01:05 tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:",
		"2026/10/03 20:04:05 tier_sweep: gastown: integration finished (exit 0) in 3m0s; last lines:",
		"2026/10/03 21:00:00 tier_sweep: gastown: sweep started b4c5d6e7 (shell)",
		"2026/10/03 21:00:30 tier_sweep: gastown: shell finished (exit 0) in 30s; last lines:",
		"2026/10/03 21:01:00 tier_sweep: gastown: swept b4c5d6e7 (shell GREEN) in 1m0s",
	)
	ts := r.read(time.Date(2026, 10, 3, 23, 0, 0, 0, time.Local))
	if len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v, want the one swept cycle", ts.Sweeps)
	}
	if walls := ts.Sweeps[0].Walls; walls["shell"] != 30 || len(walls) != 1 {
		t.Errorf("walls = %v, want only the newer cycle's shell 30s", walls)
	}
}
