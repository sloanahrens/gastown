package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// doltArchiveName is the job list's row for the history archive, which the
// scheduled_maintenance patrol writes one dolt-log-<db>-<date>.csv.gz per
// database per UTC day.
const doltArchiveName = "dolt_archive"

// doltArchiveRows returns the dolt_archive row for the directory
// patrols.scheduled_maintenance.archive_dir names, or no row when the config
// names none: the daemon reads an unset directory as archiving off, so nothing
// is due. The last run is the newest dolt-log archive file's modification time
// on the directory's top level. A config value that cannot be read or is not an
// absolute path is unknown, as is a directory that cannot be read; a directory
// with no archive file is never_run.
func doltArchiveRows(townRoot string, now time.Time) []Job {
	j := Job{Name: doltArchiveName, Kind: config.DaemonStepPatrol, Cadence: nightlyCadence}
	dir, named, err := archiveDir(townRoot)
	switch {
	case err != nil:
		j.State = StateUnknown
		return []Job{j}
	case !named:
		return nil
	}
	// os.DirFS names paths relative to its root, so the archive directory is
	// the root and its top level is ".".
	last, err := reconcile.ArchiveLastGood(os.DirFS(dir), ".")
	switch {
	case errors.Is(err, reconcile.ErrNoResult):
		j.State = StateNeverRun
	case err != nil:
		j.State = StateUnknown
	default:
		j.LastRun = &last
		j.Outcome = OutcomeOK
		j.State = stateOf(j, false, false, now)
	}
	return []Job{j}
}

// archiveDir returns the history archive's directory as
// patrols.scheduled_maintenance.archive_dir names it, read the way readCadences
// reads the patrol config so the two-file section rule applies. named is false
// when the config names no directory. An error is a config or value this list
// cannot read, which must never pass as off.
func archiveDir(townRoot string) (dir string, named bool, err error) {
	var doc map[string]json.RawMessage
	if err := config.CheckJSONFileParses(config.DaemonPatrolConfigPath(townRoot), &doc); err != nil {
		return "", false, fmt.Errorf("read daemon patrol config: %w", err)
	}
	var patrols map[string]json.RawMessage
	if raw, has := doc["patrols"]; has {
		if err := json.Unmarshal(raw, &patrols); err != nil {
			return "", false, fmt.Errorf("read daemon patrol config: patrols is not an object: %w", err)
		}
	}
	raw, has := patrols["scheduled_maintenance"]
	if !has {
		return "", false, nil
	}
	var entry struct {
		ArchiveDir *string `json:"archive_dir"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", false, fmt.Errorf("read %s: %w", config.MaintenanceArchiveDirKey, err)
	}
	if entry.ArchiveDir == nil {
		return "", false, nil
	}
	section := config.ScheduledMaintenanceConfig{ArchiveDir: *entry.ArchiveDir}
	if dir, err = section.ParseArchiveDir(); err != nil {
		return "", false, err
	}
	return dir, dir != "", nil
}
