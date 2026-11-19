package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/update"
)

const (
	// updateJobName is the daemon patrol that runs gt update.
	updateJobName = "update"
	// doltBackupJobName is the nightly backup's row: the scheduled_maintenance
	// patrol takes it once per night, so it has no patrol key of its own.
	doltBackupJobName = "dolt_backup"
	// nightlyCadence is the period of the daily windows the update trigger
	// (internal/daemon/update_due.go) and the maintenance patrol run in.
	nightlyCadence = 24 * time.Hour
)

// updateOutcomeTable maps every outcome the update package defines to the job
// row's outcome. Only a run that finished ok is ok; a run that did not finish
// cleanly did not succeed.
var updateOutcomeTable = map[update.Outcome]Outcome{
	update.OutcomeOK:          OutcomeOK,
	update.OutcomeRunning:     OutcomeFailed,
	update.OutcomePartial:     OutcomeFailed,
	update.OutcomeAborted:     OutcomeFailed,
	update.OutcomeFailed:      OutcomeFailed,
	update.OutcomeInterrupted: OutcomeFailed,
}

// updateOutcome is the job row's outcome for an update run. An outcome the
// table does not know is failed: a run this list cannot read did not succeed.
func updateOutcome(o update.Outcome) Outcome {
	if mapped, ok := updateOutcomeTable[o]; ok {
		return mapped
	}
	return OutcomeFailed
}

// updateHistory returns the newest finished update run, if any. It is the file
// the update runner appends to; a missing file is an empty history.
func updateHistory(townRoot string) ([]update.HistoryLine, error) {
	return update.ReadHistory(update.Dir(townRoot), 1)
}

// applyUpdateRun fills the update row from the newest finished update run and
// returns its state. An unreadable history or a run with no end time is
// unknown: neither may show as never_run or ok.
func applyUpdateRun(j *Job, history []update.HistoryLine, historyErr error, now time.Time) State {
	if historyErr == nil && len(history) > 0 {
		run := history[0]
		if run.EndedAt.IsZero() {
			return StateUnknown
		}
		at := run.EndedAt
		j.LastRun = &at
		j.Outcome = updateOutcome(run.Outcome)
		var total int64
		for _, ms := range run.DurationsMS {
			total += ms
		}
		j.Stats = map[string]int64{"duration_ms": total}
	}
	return stateOf(*j, historyErr != nil, false, now)
}

// backupDateLayout is the name layout doltbackup.Dir gives a night's directory.
// doltbackup does not export it, so the row repeats it to name a night the way
// doltbackup does; the row picks the newest night by that name, as the daemon
// writes one directory per date.
const backupDateLayout = "2006-01-02"

// isBackupNight reports whether name is a night's date: the test
// doltbackup.List makes before it reads a directory.
func isBackupNight(name string) bool {
	_, err := time.Parse(backupDateLayout, name)
	return err == nil
}

// readBackupManifest reads the manifest doltbackup.Commit writes last into a
// night's directory.
func readBackupManifest(dir string) (doltbackup.Manifest, error) {
	var m doltbackup.Manifest
	path := filepath.Join(dir, doltbackup.ManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// newestBackupNight reads the manifest of the newest night under root, newest
// by its date name. It does not use doltbackup.List, which drops a night whose
// manifest does not parse: that would let the row report a healthy older night
// while the newest backup is corrupt. night reports whether a date-named
// directory exists; when it does and the manifest cannot be read, the error
// says so. An empty root is an error, not a missing root: the caller did not
// resolve the backup root, and reading that as "no backups" would show
// never_run for a root nobody looked at.
func newestBackupNight(root string) (m doltbackup.Manifest, night bool, err error) {
	if root == "" {
		return m, false, errors.New("backup root is empty")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	newest := ""
	for _, e := range entries {
		if e.IsDir() && isBackupNight(e.Name()) && e.Name() > newest {
			newest = e.Name()
		}
	}
	if newest == "" {
		return m, false, nil
	}
	m, err = readBackupManifest(filepath.Join(root, newest))
	return m, true, err
}

// doltBackupRow is the nightly backup's row: when the newest night finished and
// how many databases it holds. A night whose manifest cannot be read, an
// unreadable root, and a resolved root with no night each make it unknown; a
// root with no night on disk is never_run.
func doltBackupRow(m doltbackup.Manifest, night bool, readErr error, now time.Time) Job {
	j := Job{Name: doltBackupJobName, Kind: config.DaemonStepPatrol, Cadence: nightlyCadence}
	switch {
	case readErr != nil:
		j.State = StateUnknown
		if night {
			// The newest night is on disk but its manifest cannot be read:
			// that night's backup did not succeed, whatever older nights hold.
			j.Outcome = OutcomeFailed
		}
	case !night:
		j.State = StateNeverRun
	case m.Finished.IsZero():
		j.State = StateUnknown
	default:
		at := m.Finished
		j.LastRun = &at
		j.Outcome = OutcomeOK
		j.Stats = map[string]int64{"databases": int64(len(m.Databases))}
		j.State = stateOf(j, false, false, now)
	}
	return j
}
