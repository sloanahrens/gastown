package daemon

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/jobledger"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// cacheTrimStatsLog is the one line cmd/sweep-run writes into a run's log directory.
const cacheTrimStatsLog = "stats total_bytes=100 planned_files=10 planned_bytes=1000 removed_files=4 removed_bytes=400 absent_files=1 failed_removals=0 young_bytes=20 protected_bytes=30 over_bytes=0 cap_bytes=5000 elapsed_ms=9000\n"

// TestTierSweepCacheStats pins the sweep_cache_trim ledger run a finished sweep
// run's cache trim figures become, and nothing for a run that left no line (gt-oh2ywo).
func TestTierSweepCacheStats(t *testing.T) {
	t.Parallel()

	// foldStage folds one finished runner stage over a directory holding the stats line when withStats.
	foldStage := func(t *testing.T, now time.Time, withStats bool) (*Daemon, string) {
		t.Helper()
		d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
		d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
		st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA}
		spec, ok, err := d.tierSweepRunnerSpec(st)
		if err != nil || !ok {
			t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
		}
		if withStats {
			logDir := filepath.Join(spec.OutDir, sweeprunner.LogsDirName)
			mustMkdirAll(t, logDir)
			writeFile(t, filepath.Join(logDir, sweeprunner.CacheTrimStatsFileName), cacheTrimStatsLog)
		}
		d.tierSweepRunnerStageResult(st, spec, runnerTestResult(spec, now), tierSweepLaunch{exitCode: -1}, now)
		return d, logs.String()
	}

	t.Run("a recorded trim appends one run with its figures and elapsed-derived start", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, _ := foldStage(t, now, true)
		runs := cacheTrimJobRuns(t, d.config.TownRoot)
		if len(runs) != 1 {
			t.Fatalf("the ledger holds %d sweep_cache_trim run(s), want one: %+v", len(runs), runs)
		}
		got := runs[0]
		want := map[string]int64{"claim_bytes_before": 100, "planned_files": 10, "planned_bytes": 1000, "removed_files": 4, "removed_bytes": 400, "absent_files": 1, "failed_removals": 0, "young_bytes": 20, "protected_bytes": 30, "over_bytes": 0, "cap_bytes": 5000, "elapsed_ms": 9000}
		if got.Job != "sweep_cache_trim" || got.Outcome != "ok" || got.NextDue != nil || !maps.Equal(got.Stats, want) {
			t.Errorf("run = %+v with stats %v; want job sweep_cache_trim, outcome ok, no next due, stats %v", got, got.Stats, want)
		}
		if start := now.Add(-9 * time.Second); !got.StartedAt.Equal(start) || !got.EndedAt.Equal(now) {
			t.Errorf("run ran %v to %v, want %v to %v", got.StartedAt, got.EndedAt, start, now)
		}
	})

	t.Run("a run with no stats line records nothing and logs nothing", func(t *testing.T) {
		t.Parallel()
		d, logs := foldStage(t, atHour(time.Now(), 15), false)
		if runs := cacheTrimJobRuns(t, d.config.TownRoot); len(runs) != 0 {
			t.Errorf("the ledger holds %d run(s) for a run with no stats line, want none: %+v", len(runs), runs)
		}
		if logs != "" {
			t.Errorf("a run with no stats line logged:\n%s", logs)
		}
	})
}

func cacheTrimJobRuns(t *testing.T, townRoot string) []jobledger.Run {
	t.Helper()
	ledger, err := jobledger.NewLedger(townRoot)
	if err != nil {
		t.Fatalf("jobledger.NewLedger: %v", err)
	}
	runs, err := ledger.Tail("sweep_cache_trim", 10)
	if err != nil {
		t.Fatalf("ledger.Tail(sweep_cache_trim): %v", err)
	}
	return runs
}

// TestTierSweepCacheStatsUnrecorded pins a run whose cache trim figures cannot be
// recorded (gt-o5z0rv): a refused read or append logs one line, records nothing,
// and leaves the stage result and attention as a recorded run does.
func TestTierSweepCacheStatsUnrecorded(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	ledgerFile := filepath.FromSlash(".runtime/jobs/runs.jsonl")
	logsGroup := regexp.MustCompile(`\(logs [^)]*\)`)

	// fold breaks the run's log directory or town, then folds the stage. The "(logs <dir>)" group names each test's own temporary town, so it is set aside.
	fold := func(t *testing.T, breakRun func(t *testing.T, town, logDir string)) (string, tierSweepStageResult, []attention.Item, string) {
		d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
		d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
		st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA}
		spec, ok, err := d.tierSweepRunnerSpec(st)
		if err != nil || !ok {
			t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
		}
		logDir := filepath.Join(spec.OutDir, sweeprunner.LogsDirName)
		mustMkdirAll(t, logDir)
		breakRun(t, d.config.TownRoot, logDir)
		res := d.tierSweepRunnerStageResult(st, spec, runnerTestResult(spec, now), tierSweepLaunch{exitCode: -1}, now)
		res.output = logsGroup.ReplaceAllString(res.output, "(logs)")
		return d.config.TownRoot, res, d.cacheTrimNoOp.observed(), logs.String()
	}
	withStats := func(t *testing.T, _, logDir string) {
		writeFile(t, filepath.Join(logDir, sweeprunner.CacheTrimStatsFileName), cacheTrimStatsLog)
	}

	// The recorded run is the baseline every failure row is compared with.
	_, healthy, healthyItems, healthyLog := fold(t, withStats)
	if healthyLog != "" || len(healthyItems) != 0 {
		t.Fatalf("a recorded run logged %q and raised attention items %+v", healthyLog, healthyItems)
	}

	for _, r := range []struct {
		name, names string
		breakRun    func(t *testing.T, town, logDir string)
		records     func(t *testing.T, town string) int
	}{
		{
			name:  "an unreadable stats file logs one line naming it and records nothing",
			names: sweeprunner.CacheTrimStatsFileName,
			// A directory where the stats file belongs is not a regular file, so it is refused unread.
			breakRun: func(t *testing.T, _, logDir string) {
				mustMkdirAll(t, filepath.Join(logDir, sweeprunner.CacheTrimStatsFileName))
			},
			records: func(t *testing.T, town string) int { return len(cacheTrimJobRuns(t, town)) },
		},
		{
			name:  "a ledger that refuses the append logs one line and records nothing",
			names: "jobledger",
			// A directory at the live ledger file makes the append's read of the tail fail.
			breakRun: func(t *testing.T, town, logDir string) {
				withStats(t, town, logDir)
				mustMkdirAll(t, filepath.Join(town, ledgerFile))
			},
			records: func(t *testing.T, town string) int {
				entries, err := os.ReadDir(filepath.Join(town, ledgerFile))
				if err != nil {
					t.Fatalf("reading the refused ledger: %v", err)
				}
				return len(entries)
			},
		},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			town, res, items, logs := fold(t, r.breakRun)
			if strings.Count(logs, "\n") != 1 || !strings.Contains(logs, "the run's cache trim figures were not recorded") || !strings.Contains(logs, r.names) {
				t.Errorf("log = %q, want one line that says the figures were not recorded and names %q", logs, r.names)
			}
			if n := r.records(t, town); n != 0 {
				t.Errorf("the ledger holds %d record(s) for the failed figures, want none", n)
			}
			if !reflect.DeepEqual(res, healthy) {
				t.Errorf("stage result = %+v, want the recorded run's %+v", res, healthy)
			}
			if len(items) != len(healthyItems) {
				t.Errorf("attention items = %+v, want the recorded run's %+v", items, healthyItems)
			}
		})
	}
}
