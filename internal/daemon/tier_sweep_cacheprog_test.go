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

	"github.com/steveyegge/gastown/internal/jobledger"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// Stats lines as the cache program appends them: one with no eviction, one with the eviction figures.
const (
	tierSweepCacheProgCountsLine = `{"gets":3,"hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}` + "\n"
	tierSweepCacheProgEvictLine  = `{"gets":2,"hits":1,"misses":1,"puts":1,"put_bytes":100,"hit_bytes":50,"errors":0,"elapsed_ms":40,"entries":9,"total_bytes":1000,"young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450,"evict_ms":12}` + "\n"
)

// tierSweepCacheProgFold runs a finished sweep stage through tierSweepRunnerStageResult, the production
// call that records the run, after prepare sets up its log directory. It returns the stage result with the
// "(logs <dir>)" group set aside, since each case has its own town, then the log text and the town root.
func tierSweepCacheProgFold(t *testing.T, now time.Time, prepare func(t *testing.T, town, logDir string)) (tierSweepStageResult, string, string) {
	t.Helper()
	d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
	st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA}
	spec, ok, err := d.tierSweepRunnerSpec(st)
	if err != nil || !ok {
		t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
	}
	logDir := filepath.Join(spec.OutDir, sweeprunner.LogsDirName)
	mustMkdirAll(t, logDir)
	if prepare != nil {
		prepare(t, d.config.TownRoot, logDir)
	}
	res := d.tierSweepRunnerStageResult(st, spec, runnerTestResult(spec, now), tierSweepLaunch{exitCode: -1}, now)
	res.output = regexp.MustCompile(`\(logs [^)]*\)`).ReplaceAllString(res.output, "(logs)")
	return res, logs.String(), d.config.TownRoot
}

// tierSweepCacheProgRuns returns the ledger runs of one job in a town.
func tierSweepCacheProgRuns(t *testing.T, townRoot, job string) []jobledger.Run {
	t.Helper()
	ledger, err := jobledger.NewLedger(townRoot)
	if err != nil {
		t.Fatalf("jobledger.NewLedger: %v", err)
	}
	runs, err := ledger.Tail(job, 10)
	if err != nil {
		t.Fatalf("ledger.Tail(%s): %v", job, err)
	}
	return runs
}

// TestTierSweepCacheProgRecords pins the sweep_gocacheprog run a stats file becomes, and the
// sweep_gocacheprog_store run only when an eviction is in the file (gt-hvs5gd).
func TestTierSweepCacheProgRecords(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	writeStats := func(body string) func(*testing.T, string, string) {
		return func(t *testing.T, _, logDir string) {
			writeFile(t, filepath.Join(logDir, tierSweepCacheProgStatsName), body)
		}
	}

	t.Run("a counts-only file appends the counts run and no store run", func(t *testing.T) {
		t.Parallel()
		_, _, town := tierSweepCacheProgFold(t, now, writeStats(tierSweepCacheProgCountsLine))
		want := map[string]int64{"processes": 1, "bad_lines": 0, "gets": 3, "hits": 2, "misses": 1, "puts": 4, "put_bytes": 400, "hit_bytes": 200, "errors": 0, "elapsed_ms": 1500}
		if runs := tierSweepCacheProgRuns(t, town, "sweep_gocacheprog"); len(runs) != 1 || runs[0].Outcome != "ok" || !maps.Equal(runs[0].Stats, want) {
			t.Errorf("sweep_gocacheprog runs = %+v; want one ok run with stats %v", runs, want)
		}
		if runs := tierSweepCacheProgRuns(t, town, "sweep_gocacheprog_store"); len(runs) != 0 {
			t.Errorf("the ledger holds %d store run(s) with no eviction in the file, want none: %+v", len(runs), runs)
		}
	})

	t.Run("an eviction in the file appends the counts run and the store run", func(t *testing.T) {
		t.Parallel()
		_, _, town := tierSweepCacheProgFold(t, now, writeStats(tierSweepCacheProgEvictLine))
		counts := map[string]int64{"processes": 1, "bad_lines": 0, "gets": 2, "hits": 1, "misses": 1, "puts": 1, "put_bytes": 100, "hit_bytes": 50, "errors": 0, "elapsed_ms": 40}
		store := map[string]int64{"evictions": 1, "evicted_entries": 3, "evicted_bytes": 450, "evict_ms": 12, "entries": 9, "claim_bytes_before": 1000, "young_bytes": 300, "cap_bytes": 800, "over_in_use_bytes": 200}
		if runs := tierSweepCacheProgRuns(t, town, "sweep_gocacheprog"); len(runs) != 1 || !maps.Equal(runs[0].Stats, counts) {
			t.Errorf("sweep_gocacheprog runs = %+v; want one with stats %v", runs, counts)
		}
		if runs := tierSweepCacheProgRuns(t, town, "sweep_gocacheprog_store"); len(runs) != 1 || !maps.Equal(runs[0].Stats, store) {
			t.Errorf("sweep_gocacheprog_store runs = %+v; want one with stats %v", runs, store)
		}
	})
}

// TestTierSweepCacheProgUnrecorded pins what an absent stats file and a file that cannot be recorded do
// (gt-hvs5gd): an absent file is silent, and a read error or a refused append logs one line and records
// nothing. The stage result matches a run with no file.
func TestTierSweepCacheProgUnrecorded(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	baseline, logs, town := tierSweepCacheProgFold(t, now, nil)
	if logs != "" || len(tierSweepCacheProgRuns(t, town, "sweep_gocacheprog")) != 0 {
		t.Fatalf("an absent stats file logged %q or recorded runs; want neither", logs)
	}

	rows := []struct {
		name, names string
		prepare     func(t *testing.T, town, logDir string)
		// records counts the ledger entries the failed figures left in town.
		records func(t *testing.T, town string) int
	}{
		{
			name:    "a link where the stats file belongs",
			names:   tierSweepCacheProgStatsName,
			prepare: func(t *testing.T, _, logDir string) { symlinkTo(t, logDir, tierSweepCacheProgStatsName) },
			records: func(t *testing.T, town string) int { return len(tierSweepCacheProgRuns(t, town, "sweep_gocacheprog")) },
		},
		{
			name:  "a directory where the stats file belongs",
			names: tierSweepCacheProgStatsName,
			prepare: func(t *testing.T, _, logDir string) {
				mustMkdirAll(t, filepath.Join(logDir, tierSweepCacheProgStatsName))
			},
			records: func(t *testing.T, town string) int { return len(tierSweepCacheProgRuns(t, town, "sweep_gocacheprog")) },
		},
		{
			name:  "a ledger that refuses the append",
			names: "jobledger",
			prepare: func(t *testing.T, town, logDir string) {
				writeFile(t, filepath.Join(logDir, tierSweepCacheProgStatsName), tierSweepCacheProgEvictLine)
				// A directory at the live ledger file makes every append's read of it fail.
				mustMkdirAll(t, filepath.Join(town, ".runtime", "jobs", "runs.jsonl"))
			},
			records: func(t *testing.T, town string) int {
				entries, err := os.ReadDir(filepath.Join(town, ".runtime", "jobs", "runs.jsonl"))
				if err != nil {
					t.Fatalf("reading the refused ledger: %v", err)
				}
				return len(entries)
			},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			res, logs, town := tierSweepCacheProgFold(t, now, r.prepare)
			if strings.Count(logs, "\n") != 1 || !strings.Contains(logs, "the run's cache program figures were not recorded") || !strings.Contains(logs, r.names) {
				t.Errorf("log = %q, want one line that says the figures were not recorded and names %q", logs, r.names)
			}
			if n := r.records(t, town); n != 0 {
				t.Errorf("the ledger holds %d record(s) for the failed figures, want none", n)
			}
			if !reflect.DeepEqual(res, baseline) {
				t.Errorf("stage result = %+v, want the no-file run's %+v", res, baseline)
			}
		})
	}
}

// symlinkTo makes name in dir a link to a stats file elsewhere in the test's own directory.
func symlinkTo(t *testing.T, dir, name string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	writeFile(t, target, tierSweepCacheProgCountsLine)
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}
