package jobs

import (
	"errors"
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

// backupNights returns the nights doltbackup.List finds under root. An empty
// root is an error, not a missing root: the caller did not resolve the backup
// root, and reading that as "no backups" would show never_run for a root
// nobody looked at.
func backupNights(root string) ([]doltbackup.Backup, error) {
	if root == "" {
		return nil, errors.New("backup root is empty")
	}
	return doltbackup.List(root)
}

// doltBackupRow is the nightly backup's row: when the newest night finished and
// how many databases it holds. An unreadable root, or nights with no finish
// time, make it unknown; no night on disk is never_run.
func doltBackupRow(backups []doltbackup.Backup, readErr error, now time.Time) Job {
	j := Job{Name: doltBackupJobName, Kind: config.DaemonStepPatrol, Cadence: nightlyCadence}
	switch {
	case readErr != nil:
		j.State = StateUnknown
	case len(backups) == 0:
		j.State = StateNeverRun
	default:
		newest := backups[0]
		for _, b := range backups[1:] {
			if b.Manifest.Finished.After(newest.Manifest.Finished) {
				newest = b
			}
		}
		if newest.Manifest.Finished.IsZero() {
			j.State = StateUnknown
			break
		}
		at := newest.Manifest.Finished
		j.LastRun = &at
		j.Outcome = OutcomeOK
		j.Stats = map[string]int64{"databases": int64(len(newest.Manifest.Databases))}
		j.State = stateOf(j, false, false, now)
	}
	return j
}
