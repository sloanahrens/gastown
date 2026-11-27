package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// dutyInputs are the reads one observation pass makes. The backup duty reads
// databases, backupRoot and fsys; the archive duty reads archiveDir and fsys;
// the gc duty reuses databases and reads readGCState; the other fields feed the
// duties observed later. backupErr and archiveErr are the seam's own failure:
// they are judged as unknown rather than letting an empty path read as "off".
type dutyInputs struct {
	databases   func(ctx context.Context) ([]string, error)
	backupRoot  string
	backupErr   error
	archiveDir  string // absolute; empty means archiving is off
	archiveErr  error
	readGCState func() ([]byte, error)
	fsys        fs.FS // os.DirFS("/") in production
}

// observeDuties judges the maintenance duties against the live inputs one pass
// reads: the server's database list, the backup root, the archive setting, the
// gc state and the town-state root. A seam that fails is carried into the
// judgement as its error, so it reads unknown (or overdue with no result when
// nothing can exist yet), never fresh. gt-m4ubr4 calls it from the pass.
func (d *Daemon) observeDuties(ctx context.Context, now time.Time) []reconcile.Observation {
	pc := d.patrol()
	var cfg *config.ReconcilerConfig
	if pc != nil && pc.Patrols != nil {
		cfg = pc.Patrols.Reconciler
	}
	m := d.maintenance()
	// A daemon without a town root reads no state, rather than a path relative
	// to its working directory.
	readGCState := func() ([]byte, error) {
		if d.config == nil || d.config.TownRoot == "" {
			return nil, errors.New("no town root to read the maintenance state from")
		}
		return os.ReadFile(maintenanceGCStatePath(d.config.TownRoot))
	}
	in := dutyInputs{
		databases:   func(ctx context.Context) ([]string, error) { return m.serverDatabases(d) },
		readGCState: readGCState,
		fsys:        os.DirFS("/"),
	}
	in.backupRoot, in.backupErr = m.backupRoot()
	in.archiveDir, in.archiveErr = d.maintenanceArchiveDir()
	root, rootErr := d.townStateNightlyRoot()
	return append(observeDatabaseDuties(ctx, now, cfg, in), observeTownStateDuty(now, cfg, root, rootErr, in.fsys)...)
}

// observeDatabaseDuties judges the maintenance duties at now against the limits
// and grace in cfg. The databases are listed once for the backup and gc duties;
// the archive duty is judged too when archiving is on or its setting was
// refused. A failed listing makes the backup and gc observations unknown and
// does not stop the archive read.
func observeDatabaseDuties(ctx context.Context, now time.Time, cfg *config.ReconcilerConfig, in dutyInputs) []reconcile.Observation {
	var databases []string
	var listErr error
	if in.databases == nil {
		listErr = errors.New("no database listing to read")
	} else {
		databases, listErr = in.databases(ctx)
	}
	obs := []reconcile.Observation{observeBackupDuty(now, cfg, in, databases, listErr)}
	if in.archiveDir != "" || in.archiveErr != nil {
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

// observeTownStateDuty judges the nightly town-state backup duty from the root
// townStateNightlyRoot read and the nights under it. An explicitly empty root
// (errTownStateNightlyOff) is the operator's off, so nothing is observed. Any
// other root error (unset, relative, no section) reads overdue with ErrNoResult:
// no root means no night can exist. With a root, the newest night's archive
// decides, and its read error is judged as that error, never as fresh.
func observeTownStateDuty(now time.Time, cfg *config.ReconcilerConfig, root string, rootErr error, fsys fs.FS) []reconcile.Observation {
	if errors.Is(rootErr, errTownStateNightlyOff) {
		return nil
	}
	duty := reconcile.Duty{Name: config.ReconcilerDutyTownState}
	if rootErr != nil {
		return []reconcile.Observation{reconcile.Evaluate(duty, now, time.Time{}, reconcile.ErrNoResult)}
	}
	fail := func(err error) []reconcile.Observation {
		return []reconcile.Observation{reconcile.Evaluate(duty, now, time.Time{}, err)}
	}
	limit, err := cfg.ParseLimit(config.ReconcilerDutyTownState)
	if err != nil {
		return fail(err)
	}
	grace, err := cfg.ParseGrace()
	if err != nil {
		return fail(err)
	}
	duty.Limit, duty.Grace = limit, grace
	if fsys == nil {
		return fail(errors.New("no filesystem to read the town-state nights from"))
	}
	// os.DirFS("/") names paths without the leading slash.
	rel := strings.TrimPrefix(filepath.Clean(root), "/")
	if rel == "" {
		rel = "."
	}
	lastGood, err := reconcile.TownStateLastGood(fsys, rel)
	return []reconcile.Observation{reconcile.Evaluate(duty, now, lastGood, err)}
}

// observeBackupDuty judges the backup duty from the newest complete dated
// backup night. A failed listing, a failed backup-root read, a refused setting,
// a relative root and a missing filesystem are kept as the read error, so none
// of them reads as fresh.
func observeBackupDuty(now time.Time, cfg *config.ReconcilerConfig, in dutyInputs, databases []string, listErr error) reconcile.Observation {
	duty := reconcile.Duty{Name: config.ReconcilerDutyBackup}
	fail := func(err error) reconcile.Observation {
		return reconcile.Evaluate(duty, now, time.Time{}, err)
	}
	if listErr != nil {
		return fail(listErr)
	}
	if in.backupErr != nil {
		return fail(in.backupErr)
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

// observeArchiveDuty judges the archive duty. A failed archive-setting read, a
// refused setting, a relative directory and a missing filesystem reach
// reconcile.Evaluate as its read error, so none of them reads as fresh; a read
// error is judged by Evaluate as it says.
func observeArchiveDuty(now time.Time, cfg *config.ReconcilerConfig, in dutyInputs) reconcile.Observation {
	duty := reconcile.Duty{Name: config.ReconcilerDutyArchive}
	fail := func(err error) reconcile.Observation {
		return reconcile.Evaluate(duty, now, time.Time{}, err)
	}
	if in.archiveErr != nil {
		return fail(in.archiveErr)
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
