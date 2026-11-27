package jobs

import (
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// dutyPrefix names each duty row's job; the reconciler's duty name follows it.
const dutyPrefix = "duty:"

// dutyRows returns one row per reconciler duty whose verdict is ok, due or
// overdue, from the record the reconciler pass writes under the town root. An
// unknown verdict or an unreadable record gives no rows here; gt-pro7tt adds
// the rows that raise instead (gt-p11xsh). The reading time is not read: the
// reconciler judged each verdict when it wrote the record.
func dutyRows(townRoot string, _ time.Time) []Job {
	record, err := daemon.ReadDutyState(townRoot)
	if err != nil {
		return nil
	}
	var rows []Job
	for _, d := range record.Duties {
		j := Job{Name: dutyPrefix + d.Name, Kind: config.DaemonStepPatrol, Cadence: time.Duration(d.LimitSeconds) * time.Second}
		switch d.Verdict {
		case reconcile.VerdictOK, reconcile.VerdictDue:
			j.State, j.Outcome = StateOK, OutcomeOK
		case reconcile.VerdictOverdue:
			j.State = StateOverdue
		default:
			continue
		}
		if lastGood := d.LastGood; !lastGood.IsZero() {
			j.LastRun = &lastGood
		}
		rows = append(rows, j)
	}
	return rows
}
