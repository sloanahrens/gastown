package jobs

import (
	"errors"
	"io/fs"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// dutyPrefix names each duty row's job; the reconciler's duty name follows it.
const dutyPrefix = "duty:"

// dutyRecordRow names the row for a duty state record that exists but cannot be
// read: no duty's verdict survives, so one row says so.
const dutyRecordRow = dutyPrefix + "record"

// dutyStaleAfter is how old the reconciler's record may be before its duties
// are unreadable. The pass rewrites the record each time it runs, so an older
// record says the pass stopped, not that every duty is fine.
const dutyStaleAfter = 30 * time.Minute

// dutyRows returns one row per duty in the record the reconciler pass writes
// under the town root. A verdict of ok, due or overdue keeps its state; an
// unknown verdict, and every duty in a record older than dutyStaleAfter, raises
// unknown with a failed outcome, because neither is ok. An absent record is no
// duties recorded yet, so no rows. A record that exists but cannot be read is
// the one failed duty:record row, as doltGCRows does for an unreadable state.
func dutyRows(townRoot string, now time.Time) []Job {
	record, err := daemon.ReadDutyState(townRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []Job{{Name: dutyRecordRow, Kind: config.DaemonStepPatrol, State: StateUnknown, Outcome: OutcomeFailed}}
	}
	stale := now.Sub(record.At) > dutyStaleAfter
	var rows []Job
	for _, d := range record.Duties {
		j := Job{Name: dutyPrefix + d.Name, Kind: config.DaemonStepPatrol, Cadence: time.Duration(d.LimitSeconds) * time.Second}
		switch {
		case stale:
			j.State, j.Outcome = StateUnknown, OutcomeFailed
		case d.Verdict == reconcile.VerdictOK, d.Verdict == reconcile.VerdictDue:
			j.State, j.Outcome = StateOK, OutcomeOK
		case d.Verdict == reconcile.VerdictOverdue:
			j.State = StateOverdue
		default:
			// VerdictUnknown, and any verdict this list does not know, is a
			// duty whose state cannot be called ok.
			j.State, j.Outcome = StateUnknown, OutcomeFailed
		}
		if lastGood := d.LastGood; !lastGood.IsZero() {
			j.LastRun = &lastGood
		}
		rows = append(rows, j)
	}
	return rows
}
