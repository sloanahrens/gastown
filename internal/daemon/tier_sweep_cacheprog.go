package daemon

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/jobledger"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

const (
	// tierSweepCacheProgStatsName is the copy sweep-run leaves in a run's log directory (gt-fqwlm3).
	tierSweepCacheProgStatsName = "gocacheprog-stats.jsonl"
	// tierSweepCacheProgLedgerJob and tierSweepCacheProgStoreLedgerJob name a run's cache program
	// counts and, when an eviction is in the file, its store (gt-hvs5gd).
	tierSweepCacheProgLedgerJob      = "sweep_gocacheprog"
	tierSweepCacheProgStoreLedgerJob = "sweep_gocacheprog_store"
)

// recordTierSweepCacheFigures records a finished run's cache figures in the job ledger: the trim
// figures (gt-oh2ywo) and the cache program's (gt-hvs5gd). It is the one call the stage result
// makes, so tier_sweep.go keeps its line count and the line citations in docs/design/pod-engine.md hold.
func (d *Daemon) recordTierSweepCacheFigures(rig, outDir string) {
	d.recordTierSweepCacheTrimRun(rig, outDir)
	d.recordTierSweepCacheProgRun(rig, outDir)
}

// recordTierSweepCacheProgRun appends one finished run's cache program figures to the job
// ledger: the summed counts as sweep_gocacheprog, and the store as sweep_gocacheprog_store
// only when an eviction is in the file (gt-hvs5gd). A run with no stats file records and logs
// nothing. A file that cannot be read logs one line and records nothing, and a refused append
// logs one line and stops, so the store run is not tried after a refused counts run. None of
// these changes the sweep's verdict, line or items.
func (d *Daemon) recordTierSweepCacheProgRun(rig, outDir string) {
	path := filepath.Join(outDir, sweeprunner.LogsDirName, tierSweepCacheProgStatsName)
	data, present, err := readSweepCacheProgStats(path, os.Lstat, os.ReadFile)
	switch {
	case err != nil:
		d.logger.Printf("tier_sweep: %s: the run's cache program figures were not recorded: %v", rig, err)
		return
	case !present:
		return
	}
	counts, store := sumGocacheprogStats(data)
	at := d.clk().Now()
	runs := []jobledger.Run{{Job: tierSweepCacheProgLedgerJob, StartedAt: at, EndedAt: at, Outcome: tierSweepOutcomeOK, Stats: counts}}
	if store != nil {
		runs = append(runs, jobledger.Run{Job: tierSweepCacheProgStoreLedgerJob, StartedAt: at, EndedAt: at, Outcome: tierSweepOutcomeOK, Stats: store})
	}
	for _, run := range runs {
		if err := d.appendJobRun(run); err != nil {
			d.logger.Printf("tier_sweep: %s: the run's cache program figures were not recorded: %v", rig, err)
			return
		}
	}
}
