package daemon

import (
	"context"
	"fmt"
	"time"
)

// diskMaintFailedKey is the attention item key of one target's failure: one per
// target, so a second consecutive failure records onto the item already open.
func diskMaintFailedKey(name string) string { return "disk-maint-failed:" + name }

// diskMaintConclude records a patrol's conclusion: every one sets the target's
// next run a full interval ahead, since a failure is not retried on a timer
// (gt-q9gvmy). A failure raises one item naming the label, time and why; an
// item already open raises nothing, and clearing it on a verdict is the next
// bead.
func (d *Daemon) diskMaintConclude(ctx context.Context, name, label string, verdict bool, concluded time.Time, every time.Duration, why string) {
	d.diskMaintSchedule.record(name, concluded)
	if verdict {
		return
	}
	key := diskMaintFailedKey(name)
	if d.cacheTrimNoOp.alertOpen(key) {
		return
	}
	message := fmt.Sprintf("disk maintenance %s did not reach a verdict at %s: %s; the next attempt is at its normal cadence, in %s",
		label, concluded.Format(time.RFC3339), why, every)
	// The mark follows the send: a failed send leaves the item unmarked, so the
	// next failure raises it again.
	if err := d.escalateAlertSeverityCtx(ctx, cacheTrimAlertSeverity, key, "disk_maintenance", message); err != nil {
		d.cacheTrimLogf("disk_maintenance: %s: the alert was not sent: %v", name, err)
		return
	}
	d.cacheTrimNoOp.markAlertOpen(key)
}
