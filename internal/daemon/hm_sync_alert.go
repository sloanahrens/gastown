package daemon

// hm_sync_alert is the attention half of the hm sync patrol (gt-z6qre.16):
// after every sync it reads `hm status --json`, asks the hmsync evaluator
// whether any configured source is fresh, and raises or clears hmsync:stale.
// Without it a sync that has been failing for days is one log line per interval
// that nobody reads.
//
// A status the patrol cannot read raises the same alert with the unreadable
// detail instead of leaving the last readable verdict standing: an unreadable
// index is an unknown, and an unknown is never healthy.
//
// The open flag lives in memory, keyed by the patrol rather than rebuilt from
// the open escalations at boot: a restart forgets it and re-raises an open
// alert once, which costs a repeat rather than a silence.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/hmsync"
)

const (
	// hmSyncAlertSource is the source the stale-sources alert carries: the
	// patrol that read the status.
	hmSyncAlertSource = "hm_sync"

	// hmSyncHealthyReason is what a clear records, so the operator reading the
	// closed escalation sees the patrol's verdict rather than an empty reason.
	hmSyncHealthyReason = "hm sync healthy"

	// hmSyncStatusTimeout bounds one status read. It reads an index the sync
	// just wrote, so a read still running after a minute is stuck.
	hmSyncStatusTimeout = time.Minute

	// hmSyncStatusArg and hmSyncStatusJSON are the arguments that make the
	// command report its sources instead of syncing them.
	hmSyncStatusArg  = "status"
	hmSyncStatusJSON = "--json"
)

// hmSyncAlertState is what the patrol knows of hmsync:stale. The zero value is
// unknown, which is the state after a boot.
type hmSyncAlertState int

const (
	// hmSyncAlertUnknown: an alert an earlier run raised may still be open, and
	// the patrol does not read that back.
	hmSyncAlertUnknown hmSyncAlertState = iota
	// hmSyncAlertClosed: the patrol last saw the alert cleared or never raised.
	hmSyncAlertClosed
	// hmSyncAlertOpen: the patrol raised the alert and has not cleared it.
	hmSyncAlertOpen
)

// hmSyncAlerts is the patrol's memory of whether hmsync:stale is open, and the
// two functions that carry it. A test replaces the functions with recorders;
// the nil ones are the daemon's alert helpers, resolved per call so a Daemon
// built as a literal needs no wiring.
type hmSyncAlerts struct {
	mu    sync.Mutex
	state hmSyncAlertState
	// raise delivers the alert; nil is d.escalateAlertErr.
	raise func(key, source, message string) error
	// clearFn closes it; nil is d.clearAlertsErr.
	clearFn func(reason string, keys ...string) error
}

// current returns what the patrol knows of the alert.
func (a *hmSyncAlerts) current() hmSyncAlertState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// mark records what the patrol now knows of the alert.
func (a *hmSyncAlerts) mark(state hmSyncAlertState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = state
}

// raiseHMSyncAlert delivers the stale-sources alert. The caller branches on the
// outcome: an alert recorded as raised when it never arrived closes the retry
// that would have delivered it.
func (d *Daemon) raiseHMSyncAlert(message string) error {
	raise := d.hmSyncAlerts.raise
	if raise == nil {
		raise = d.escalateAlertErr
	}
	return raise(hmsync.AlertKey, hmSyncAlertSource, message)
}

// clearHMSyncAlert closes the stale-sources alert under the same key it was
// raised with, and only that key.
func (d *Daemon) clearHMSyncAlert() error {
	clearFn := d.hmSyncAlerts.clearFn
	if clearFn == nil {
		clearFn = d.clearAlertsErr
	}
	return clearFn(hmSyncHealthyReason, hmsync.AlertKey)
}

// checkHMSyncHealth reads the status once and applies the alert change it calls
// for. It runs after every sync attempt, a failed one included: the sync that
// just failed is exactly when the sources are most likely stale.
func (d *Daemon) checkHMSyncHealth() {
	verdict, unreadable := d.hmSyncVerdict()
	if unreadable && !verdict.Raise {
		// The alert is already open, so nothing changes, but its detail is now
		// an older reading and the operator should see why it stopped updating.
		d.logger.Printf("hm_sync: %s stays open: %s", hmsync.AlertKey, verdict.Detail)
	}
	d.applyHMSyncVerdict(verdict)
}

// hmSyncVerdict runs `hm status --json` through the patrol's run seam and
// returns the change the evaluator calls for, with whether the status was
// unreadable at all. A run that fails or output that does not parse raises
// unless the alert is already open, so an unreadable status is reported once
// rather than every interval.
func (d *Daemon) hmSyncVerdict() (hmsync.Verdict, bool) {
	state := d.hmSyncAlerts.current()

	ctx, cancel := d.hmSyncStatusContext()
	defer cancel()

	command := hmSyncCommand(d.patrolConfig)
	out, err := d.hmSyncSeamsOf().run(ctx, command, hmSyncStatusArg, hmSyncStatusJSON)
	if err != nil && d.shuttingDown() {
		// The daemon is stopping, so the run was cut short, not failed. Raising
		// now would report a status the patrol never read.
		return hmsync.Verdict{}, false
	}
	if err == nil {
		var sources []hmsync.Source
		if sources, err = hmsync.ParseStatus(out); err == nil {
			return hmSyncEvaluate(sources, state), false
		}
	}
	// The detail names the failure and never the command's output, which can
	// hold a path only the operator owns.
	return hmsync.Verdict{
		Raise:  state != hmSyncAlertOpen,
		Detail: fmt.Sprintf("hm status unreadable: %v", err),
	}, true
}

// hmSyncEvaluate applies the evaluator to a readable status. The evaluator
// clears only an alert the caller knows is open, so after a boot a healthy read
// would never close an alert an earlier run left open. Unknown therefore clears
// on any healthy read: a clear with nothing open is a no-op.
func hmSyncEvaluate(sources []hmsync.Source, state hmSyncAlertState) hmsync.Verdict {
	if state != hmSyncAlertUnknown {
		return hmsync.Evaluate(sources, state == hmSyncAlertOpen)
	}
	v := hmsync.Evaluate(sources, false)
	if !v.Raise {
		v.Clear = true
	}
	return v
}

// applyHMSyncVerdict raises and clears the alert the verdict calls for, and
// records only the change that landed. A raise the notifier refused leaves the
// alert closed, so the next pass raises it again rather than treating a silence
// as an open alert; a clear that failed leaves it open for the same reason.
func (d *Daemon) applyHMSyncVerdict(v hmsync.Verdict) {
	switch {
	case v.Raise:
		if err := d.raiseHMSyncAlert(v.Detail); err != nil {
			d.logger.Printf("hm_sync: alert %s was not delivered: %v", hmsync.AlertKey, err)
			return
		}
		d.hmSyncAlerts.mark(hmSyncAlertOpen)
		d.logger.Printf("hm_sync: raised %s: %s", hmsync.AlertKey, v.Detail)
	case v.Clear:
		if err := d.clearHMSyncAlert(); err != nil {
			d.logger.Printf("hm_sync: alert %s was not cleared: %v", hmsync.AlertKey, err)
			return // not known closed, and the next pass clears it again
		}
		d.hmSyncAlerts.mark(hmSyncAlertClosed)
		d.logger.Printf("hm_sync: cleared %s: %s", hmsync.AlertKey, v.Detail)
	}
}

// hmSyncStatusContext bounds one status read and stops it when the daemon shuts
// down. The caller must cancel it: that is what releases the timer.
func (d *Daemon) hmSyncStatusContext() (context.Context, context.CancelFunc) {
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, hmSyncStatusTimeout)
}
