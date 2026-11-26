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

// dutyInputs are the reads one observation pass makes. The archive duty reads
// only archiveDir and fsys; the other fields feed the duties observed later.
type dutyInputs struct {
	databases   func(ctx context.Context) ([]string, error)
	backupRoot  string
	archiveDir  string // absolute; empty means archiving is off
	readGCState func() ([]byte, error)
	fsys        fs.FS // os.DirFS("/") in production
}

// observeDatabaseDuties judges the maintenance duties at now against the limits
// and grace in cfg. Only the archive duty is observed here: with archiving off
// there is no observation. The ctx is for the database listing a later step
// makes; the archive read does not use it.
func observeDatabaseDuties(_ context.Context, now time.Time, cfg *config.ReconcilerConfig, in dutyInputs) []reconcile.Observation {
	if in.archiveDir == "" {
		return nil
	}
	return []reconcile.Observation{observeArchiveDuty(now, cfg, in)}
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
