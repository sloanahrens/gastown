package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	defaultDoltBackupInterval = 15 * time.Minute
	// doltBackupTimeout is generous so a large commit delta on a big database
	// (e.g. hq under a wisp flood) does not blow the deadline mid-sync. The old
	// 120s ceiling produced spurious exit-1 backup failures (gt-ye21).
	doltBackupTimeout = 5 * time.Minute
	// doltBackupRetries / doltBackupRetryDelay retry a failed sync after a short
	// pause, so a transient lock (a concurrent dolt op holding the db) does not
	// fail the whole backup cycle.
	doltBackupRetries    = 1
	doltBackupRetryDelay = 5 * time.Second
)

// doltBackupInterval returns the configured backup interval, or the default (15m).
func doltBackupInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.DoltBackup != nil {
		if config.Patrols.DoltBackup.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.DoltBackup.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultDoltBackupInterval
}

// triggerDoltBackup runs a dolt_backup cycle when the patrol is due, on its
// own goroutine.
//
// The ticker that drives this call is a check cadence, not a run cadence: an
// in-process ticker resets its countdown on every daemon restart, so due-ness
// is instead decided from the persisted last-run time in
// daemon/patrol_last_run.json, which survives a restart (gt-ima2, gt-gxpwc).
//
// Dispatched onto its own goroutine, like the gt-ima2 fix for compactor_dog:
// a sync can hold for minutes on a large commit delta, and running it inline
// would stall every other tick behind it.
func (d *Daemon) triggerDoltBackup() {
	if !d.isPatrolActive("dolt_backup") {
		return
	}

	dec := evaluatePatrolDue(d.config.TownRoot, "dolt_backup", time.Time{}, time.Now(), doltBackupInterval(d.patrolConfig))
	if !dec.due {
		d.logger.Printf("dolt_backup: not due — %s", dec.note)
		return
	}

	if !d.doltBackupRunning.CompareAndSwap(false, true) {
		d.logger.Printf("dolt_backup: previous cycle still running — skipping this check")
		return
	}

	if dec.warn != "" {
		d.logger.Printf("dolt_backup: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("dolt_backup: due — %s", dec.note)
	}

	go func() {
		defer d.doltBackupRunning.Store(false)
		d.syncDoltBackups()
	}()
}

// syncDoltBackups syncs each production database to its configured backup location.
// Non-fatal: errors are logged but don't stop the daemon.
func (d *Daemon) syncDoltBackups() {
	// Dolt backup uses iCloud Drive for offsite sync — only available on macOS.
	// On Linux this generates HIGH priority escalation spam every ~15 minutes.
	if runtime.GOOS != "darwin" {
		return
	}
	if !d.isPatrolActive("dolt_backup") {
		return
	}
	release, ok := d.tryDoltTask("dolt_backup")
	if !ok {
		return
	}
	defer release()

	// Record that a cycle was attempted, regardless of outcome below — the
	// same "attempted" semantics as the ticker firing before gt-ima2/gt-gxpwc.
	// An unwritable daemon directory only means the next restart or tick
	// re-checks; it does not fail the cycle itself.
	defer func() {
		if err := savePatrolLastRun(d.config.TownRoot, "dolt_backup", time.Now()); err != nil {
			d.logger.Printf("dolt_backup: WARNING: cannot persist last-run time (%v) — "+
				"the next check may re-run sooner than expected", err)
		}
	}()

	// Pour molecule for observability (nil-safe — all methods are no-ops on nil).
	mol := d.pourDogMolecule(constants.MolDogBackup, nil)
	defer mol.close()

	// Resolve data dir: use DoltServerManager if available, else conventional path.
	var dataDir string
	if d.doltServer != nil && d.doltServer.IsEnabled() && d.doltServer.config.DataDir != "" {
		dataDir = d.doltServer.config.DataDir
	} else {
		dataDir = filepath.Join(d.config.TownRoot, ".dolt-data")
	}
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		d.logger.Printf("dolt_backup: data dir %s does not exist, skipping", dataDir)
		mol.failStep("sync", "data dir does not exist")
		return
	}

	config := d.patrolConfig.Patrols.DoltBackup
	databases := config.Databases
	if len(databases) == 0 {
		databases = d.discoverDatabasesWithBackups(dataDir)
	}

	if len(databases) == 0 {
		d.logger.Printf("dolt_backup: no databases with backup remotes found")
		mol.failStep("sync", "no databases with backup remotes")
		return
	}

	d.logger.Printf("dolt_backup: syncing %d database(s)", len(databases))

	synced := 0
	var failures []string
	for _, db := range databases {
		backupName := db + "-backup"
		if err := d.syncBackup(dataDir, db, backupName); err != nil {
			d.logger.Printf("dolt_backup: %s: sync failed: %v", db, err)
			failures = append(failures, db)
		} else {
			synced++
		}
	}

	d.logger.Printf("dolt_backup: synced %d/%d database(s)", synced, len(databases))

	if len(failures) > 0 {
		mol.failStep("sync", fmt.Sprintf("synced %d/%d, failures: %s", synced, len(databases), strings.Join(failures, "; ")))
	} else {
		mol.closeStep("sync")
	}

	// Offsite sync: rsync local backups to iCloud Drive for cloud replication.
	// This is a stopgap until proper dolt remote push is configured.
	if synced > 0 {
		d.syncOffsiteBackup()
		mol.closeStep("offsite")
	} else {
		mol.closeStep("offsite")
	}

	mol.closeStep("report")
}

// syncBackup runs `dolt backup sync <backup-name>` for a single database,
// retrying once on failure so a transient lock or large delta does not fail the
// cycle (gt-ye21).
func (d *Daemon) syncBackup(dataDir, db, backupName string) error {
	parentCtx := d.ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	dbDir := filepath.Join(dataDir, db)

	var lastErr error
	for attempt := 0; attempt <= doltBackupRetries; attempt++ {
		if attempt > 0 {
			d.logger.Printf("dolt_backup: %s: retry %d/%d after error: %v", db, attempt, doltBackupRetries, lastErr)
			timer := time.NewTimer(doltBackupRetryDelay)
			select {
			case <-timer.C:
			case <-parentCtx.Done():
				timer.Stop()
				return parentCtx.Err()
			}
		}

		ctx, cancel := context.WithTimeout(parentCtx, doltBackupTimeout)
		cmd := exec.CommandContext(ctx, "dolt", "backup", "sync", backupName)
		cmd.Dir = dbDir
		util.SetProcessGroup(cmd)

		output, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			d.logger.Printf("dolt_backup: %s: synced to %s", db, backupName)
			return nil
		}
		lastErr = fmt.Errorf("%s: %s", err, strings.TrimSpace(string(output)))
	}
	return lastErr
}

// syncOffsiteBackup rsyncs the local backup directory to iCloud Drive.
// iCloud automatically syncs to Apple's cloud, providing offsite replication.
// Non-fatal: if iCloud is unavailable or rsync fails, we just log and continue.
func (d *Daemon) syncOffsiteBackup() {
	backupDir := filepath.Join(d.config.TownRoot, ".dolt-backup")
	if _, err := os.Stat(backupDir); os.IsNotExist(err) {
		return
	}

	// iCloud Drive path (macOS)
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	icloudDir := filepath.Join(homeDir, "Library", "Mobile Documents", "com~apple~CloudDocs", "gt-dolt-backup")
	if err := os.MkdirAll(icloudDir, 0755); err != nil {
		d.logger.Printf("dolt_backup: offsite: cannot create iCloud dir: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rsync", "-a", "--delete", backupDir+"/", icloudDir+"/")
	util.SetDetachedProcessGroup(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("dolt_backup: offsite sync failed: %v (%s)", err, strings.TrimSpace(string(output)))
	} else {
		d.logger.Printf("dolt_backup: offsite synced to iCloud")
	}
}

// discoverDatabasesWithBackups lists databases in the data directory
// that have a <name>-backup backup remote configured.
func (d *Daemon) discoverDatabasesWithBackups(dataDir string) []string {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		d.logger.Printf("dolt_backup: error reading data dir: %v", err)
		return nil
	}

	var databases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Check if this directory has a <name>-backup configured
		backupName := name + "-backup"
		if d.hasBackupRemote(dataDir, name, backupName) {
			databases = append(databases, name)
		}
	}

	return databases
}

// hasBackupRemote checks if a database has the specified backup remote configured.
func (d *Daemon) hasBackupRemote(dataDir, db, backupName string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbDir := dataDir + "/" + db
	cmd := exec.CommandContext(ctx, "dolt", "backup")
	cmd.Dir = dbDir
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == backupName {
			return true
		}
	}
	return false
}
