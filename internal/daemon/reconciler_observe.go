package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// dutyInputs are the reads one observation pass makes. The backup duty reads
// databases, backupRoot and fsys; the archive duty reads archiveDir and fsys;
// the gc duty reuses databases and reads readGCState; the other fields feed the
// duties observed later.
type dutyInputs struct {
	databases   func(ctx context.Context) ([]string, error)
	backupRoot  string
	archiveDir  string // absolute; empty means archiving is off
	readGCState func() ([]byte, error)
	fsys        fs.FS // os.DirFS("/") in production
}

// observeDatabaseDuties judges the maintenance duties at now against the limits
// and grace in cfg. The databases are listed once for the backup and gc duties;
// the archive duty is judged too when archiving is on. A failed listing makes
// the backup and gc observations unknown and does not stop the archive read.
func observeDatabaseDuties(ctx context.Context, now time.Time, cfg *config.ReconcilerConfig, in dutyInputs) []reconcile.Observation {
	var databases []string
	var listErr error
	if in.databases == nil {
		listErr = errors.New("no database listing to read")
	} else {
		databases, listErr = in.databases(ctx)
	}
	obs := []reconcile.Observation{observeBackupDuty(now, cfg, in, databases, listErr)}
	if in.archiveDir != "" {
		obs = append(obs, observeArchiveDuty(now, cfg, in))
	}
	return append(obs, observeGCDuties(now, cfg, in, databases, listErr)...)
}

// observeGCDuties judges each listed database's gc duty from the daemon's
// maintenance state file. A missing state file is an empty map, so every
// database reads overdue; a failed listing, an unreadable or unparseable state
// and a refused setting are one duty-wide unknown observation with that error.
func observeGCDuties(now time.Time, cfg *config.ReconcilerConfig, in dutyInputs, databases []string, listErr error) []reconcile.Observation {
	duty := reconcile.Duty{Name: config.ReconcilerDutyGC}
	fail := func(err error) []reconcile.Observation {
		return []reconcile.Observation{reconcile.Evaluate(duty, now, time.Time{}, err)}
	}
	if listErr != nil {
		return fail(listErr)
	}
	limit, err := cfg.ParseLimit(config.ReconcilerDutyGC)
	if err != nil {
		return fail(err)
	}
	grace, err := cfg.ParseGrace()
	if err != nil {
		return fail(err)
	}
	duty.Limit, duty.Grace = limit, grace
	if in.readGCState == nil {
		return fail(errors.New("no gc state to read"))
	}
	data, err := in.readGCState()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	var lastGC map[string]time.Time
	if err == nil {
		if lastGC, err = reconcile.ParseGCState(data); err != nil {
			return fail(err)
		}
	}
	if lastGC == nil {
		lastGC = map[string]time.Time{}
	}
	return reconcile.GCObservations(duty, now, lastGC, databases)
}

// observeBackupDuty judges the backup duty from the newest complete dated
// backup night. A failed listing, a refused setting, a relative root and a
// missing filesystem are kept as the read error, so none of them reads as fresh.
func observeBackupDuty(now time.Time, cfg *config.ReconcilerConfig, in dutyInputs, databases []string, listErr error) reconcile.Observation {
	duty := reconcile.Duty{Name: config.ReconcilerDutyBackup}
	fail := func(err error) reconcile.Observation {
		return reconcile.Evaluate(duty, now, time.Time{}, err)
	}
	if listErr != nil {
		return fail(listErr)
	}
	limit, err := cfg.ParseLimit(config.ReconcilerDutyBackup)
	if err != nil {
		return fail(err)
	}
	grace, err := cfg.ParseGrace()
	if err != nil {
		return fail(err)
	}
	duty.Limit, duty.Grace = limit, grace
	if !filepath.IsAbs(in.backupRoot) {
		return fail(fmt.Errorf("backup root %q: want an absolute path", in.backupRoot))
	}
	if in.fsys == nil {
		return fail(errors.New("no filesystem to read the backups from"))
	}
	// os.DirFS("/") names paths without the leading slash.
	rel := strings.TrimPrefix(filepath.Clean(in.backupRoot), "/")
	if rel == "" {
		rel = "."
	}
	lastGood, err := reconcile.BackupLastGood(in.fsys, rel, databases)
	return reconcile.Evaluate(duty, now, lastGood, err)
}

// observeArchiveDuty judges the archive duty. A refused setting, a relative
// directory and a missing filesystem reach reconcile.Evaluate as its read error,
// so none of them reads as fresh; a read error is judged by Evaluate as it says.
func observeArchiveDuty(now time.Time, cfg *config.ReconcilerConfig, in dutyInputs) reconcile.Observation {
	duty := reconcile.Duty{Name: config.ReconcilerDutyArchive}
	fail := func(err error) reconcile.Observation {
		return reconcile.Evaluate(duty, now, time.Time{}, err)
	}
	limit, err := cfg.ParseLimit(config.ReconcilerDutyArchive)
	if err != nil {
		return fail(err)
	}
	grace, err := cfg.ParseGrace()
	if err != nil {
		return fail(err)
	}
	duty.Limit, duty.Grace = limit, grace
	if !filepath.IsAbs(in.archiveDir) {
		return fail(fmt.Errorf("%s = %q: want an absolute path", config.MaintenanceArchiveDirKey, in.archiveDir))
	}
	if in.fsys == nil {
		return fail(errors.New("no filesystem to read the history archive from"))
	}
	// os.DirFS("/") names paths without the leading slash.
	rel := strings.TrimPrefix(filepath.Clean(in.archiveDir), "/")
	if rel == "" {
		rel = "."
	}
	lastGood, err := reconcile.ArchiveLastGood(in.fsys, rel)
	return reconcile.Evaluate(duty, now, lastGood, err)
}
