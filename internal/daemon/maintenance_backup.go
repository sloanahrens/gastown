package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltpause"
)

// scheduled_maintenance's nightly backup (gt-8z769.5): once per night, in the
// maintenance window and before the gc, every database is copied with
// CALL dolt_backup('sync-url', 'file://...') into
// ~/gt-backups/dolt/<date>/<db> (internal/doltbackup), and rotation keeps the
// newest seven. Restoring one is docs/dolt-restore.md.
//
// The copy goes through the running server, not the files under .dolt-data: a
// Dolt backup is the server's own consistent snapshot of a database's
// branches and working set (dolt_ignored tables included), so the server
// neither stops nor restarts, and nothing but the server ever opens the live
// data dir. A pause marker covers each database's copy.
//
// Order: backup first, then gc. The gc is the one maintenance step that has
// correlated with a Dolt panic, so it only runs once that night's backup is
// on disk; a failed backup escalates and skips the gc for the night. They
// never overlap: both run on the one maintenance goroutine.

const (
	// maintenanceBackupTimeout bounds one database's dolt_backup sync-url. A
	// full copy of the largest database (gt, ~0.5 GB) takes seconds; ten
	// minutes is a hang.
	maintenanceBackupTimeout = 10 * time.Minute

	// maintenanceBackupMethod is recorded in each night's manifest.
	maintenanceBackupMethod = "CALL dolt_backup('sync-url', 'file://<dir>/<db>') through the running server"
)

// maintenanceBackup takes tonight's backup unless it is already on disk. The
// outcome reads like a gc cycle's: deferred retries on the next tick, failed
// escalates and ends the night's maintenance.
func (d *Daemon) maintenanceBackup(databases []string) maintenanceGCResult {
	s := d.maintenance()
	root, err := s.backupRoot()
	if err != nil {
		return d.maintenanceBackupFailed(fmt.Errorf("backup root: %w", err), "")
	}
	started := s.now()
	if doltbackup.Taken(root, started) {
		return maintenanceGCResult{outcome: gcOutcomeCompleted}
	}

	// Someone else's deliberate outage (an operator) is not ours to
	// overwrite or end.
	if m := s.pauseCurrent(d.config.TownRoot, started); m != nil {
		d.logger.Printf("scheduled_maintenance: backup deferred: %s", m.Message())
		return maintenanceGCResult{outcome: gcOutcomeDeferred, reason: "backup: " + m.Message()}
	}
	// Exclusive against the daemon's own Dolt tasks, as the gc is.
	if !d.doltMaintMu.TryLock() {
		d.logger.Printf("scheduled_maintenance: backup deferred: daemon Dolt task in flight")
		return maintenanceGCResult{outcome: gcOutcomeDeferred, reason: "backup: daemon Dolt task in flight"}
	}
	defer d.doltMaintMu.Unlock()

	partial := doltbackup.PartialDir(root, started)
	// A partial left by a daemon that died mid-backup is restarted from empty:
	// sync-url into a stale backup would mix two nights.
	if err := os.RemoveAll(partial); err != nil {
		return d.maintenanceBackupFailed(err, "")
	}
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return d.maintenanceBackupFailed(err, "")
	}
	d.logger.Printf("scheduled_maintenance: backup of %d database(s) to %s", len(databases), partial)

	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	for _, db := range databases {
		if err := d.maintenanceBackupOne(parent, db, filepath.Join(partial, db)); err != nil {
			if rmErr := os.RemoveAll(partial); rmErr != nil {
				d.logger.Printf("scheduled_maintenance: WARNING: cannot remove failed backup %s: %v", partial, rmErr)
			}
			return d.maintenanceBackupFailed(err, db)
		}
	}

	final, err := doltbackup.Commit(root, doltbackup.Manifest{
		Started: started, Finished: s.now(), Databases: databases, Method: maintenanceBackupMethod,
	})
	if err != nil {
		return d.maintenanceBackupFailed(fmt.Errorf("commit %s: %w", partial, err), "")
	}
	d.logger.Printf("scheduled_maintenance: backup complete: %s (%s) in %v",
		final, strings.Join(databases, ", "), s.now().Sub(started).Round(time.Millisecond))

	removed, err := doltbackup.Rotate(root, doltbackup.Keep)
	if len(removed) > 0 {
		d.logger.Printf("scheduled_maintenance: backup rotation removed %s (keeping %d)", strings.Join(removed, ", "), doltbackup.Keep)
	}
	if err != nil {
		// The night's backup is on disk; a rotation failure only costs space.
		d.logger.Printf("scheduled_maintenance: WARNING: backup rotation: %v", err)
	}
	return maintenanceGCResult{outcome: gcOutcomeCompleted}
}

// maintenanceBackupOne copies one database under its own pause marker. The
// marker is removed whatever the outcome; one the daemon cannot remove lapses
// at its until.
func (d *Daemon) maintenanceBackupOne(parent context.Context, db, dest string) error {
	s := d.maintenance()
	start := s.now()
	marker := doltpause.Marker{
		Actor:  maintenancePauseActor,
		Reason: fmt.Sprintf("nightly backup of %s", db),
		Since:  start,
		Until:  start.Add(maintenanceBackupTimeout + maintenancePauseSlack),
	}
	if err := s.pauseWrite(d.config.TownRoot, marker); err != nil {
		return fmt.Errorf("cannot write pause marker, backup not run: %w", err)
	}
	defer func() {
		if _, err := s.pauseRemove(d.config.TownRoot); err != nil {
			d.logger.Printf("scheduled_maintenance: WARNING: cannot remove pause marker: %v — it lapses at %s",
				err, marker.Until.Format(time.RFC3339))
		}
	}()

	ctx, cancel := context.WithTimeout(parent, maintenanceBackupTimeout)
	defer cancel()
	if err := s.backupExec(ctx, d, db, dest); err != nil {
		return err
	}
	d.logger.Printf("scheduled_maintenance: backup %s: done in %v", db, s.now().Sub(start).Round(time.Millisecond))
	return nil
}

// maintenanceBackupFailed escalates a failed backup and ends the night's
// maintenance: the gc does not run without a fresh backup.
func (d *Daemon) maintenanceBackupFailed(err error, db string) maintenanceGCResult {
	what := "nightly backup"
	if db != "" {
		what = "nightly backup of " + db
	}
	d.logger.Printf("scheduled_maintenance: %s FAILED: %v — no gc tonight", what, err)
	d.maintenance().escalate(d, "scheduled_maintenance", fmt.Sprintf(
		"scheduled_maintenance: %s failed: %v\n"+
			"No backup was kept for tonight and the gc was skipped. The previous nights under ~/gt-backups/dolt "+
			"are untouched. Collect gt dolt status before any Dolt restart.", what, err))
	return maintenanceGCResult{outcome: gcOutcomeFailed, reason: err.Error()}
}

// doltBackupSyncURL runs CALL dolt_backup('sync-url', 'file://<dest>') for db
// through the running server, which writes the backup to dest.
func (d *Daemon) doltBackupSyncURL(ctx context.Context, db, dest string) error {
	if err := validMaintenanceDBName(db); err != nil {
		return err
	}
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("backup destination %q is not absolute", dest)
	}
	// interpolateParams: the driver quotes the URL into the statement, so a
	// prepared CALL is never needed.
	conn, err := sql.Open("mysql", d.maintenanceDSN(db, maintenanceBackupTimeout)+"&interpolateParams=true")
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	if _, err := conn.ExecContext(ctx, "CALL dolt_backup('sync-url', ?)", "file://"+filepath.ToSlash(dest)); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("dolt_backup sync-url: timeout: %w", err)
		}
		return fmt.Errorf("dolt_backup sync-url: %w", err)
	}
	return nil
}
