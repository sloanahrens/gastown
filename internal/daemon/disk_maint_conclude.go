package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// diskMaintNoReason is the failure item's text when a patrol reports no reason:
// an empty reason, or one that is only white space. It sends the operator to the
// patrol's disk_maintenance log line.
const diskMaintNoReason = "the run reached no verdict: its reason is the patrol's disk_maintenance log line at that time"

// diskMaintFailedKey is the attention item key of one target's failure: one per
// target, so a second consecutive failure records onto the item already open.
func diskMaintFailedKey(name string) string { return "disk-maint-failed:" + name }

// diskMaintReasonText is the failure item's text for a patrol's own reason: its
// first line, with carriage returns and tabs as single spaces and trimmed. A
// result longer than 300 runes is cut to 300 with "..." as the last three, and
// an empty result is diskMaintNoReason.
func diskMaintReasonText(why string) string {
	if i := strings.IndexByte(why, '\n'); i >= 0 {
		why = why[:i]
	}
	why = strings.NewReplacer("\r", " ", "\t", " ").Replace(why)
	why = strings.TrimSpace(why)
	if why == "" {
		return diskMaintNoReason
	}
	if r := []rune(why); len(r) > 300 {
		return string(r[:297]) + "..."
	}
	return why
}

// diskMaintConclude records a patrol's conclusion: every one sets the target's
// next run a full interval ahead, since a failure is not retried on a timer
// (gt-q9gvmy). A failure raises one item naming the label, time and why; an
// item already open raises nothing. A conclusion that reaches a verdict clears
// the item.
func (d *Daemon) diskMaintConclude(ctx context.Context, name, label string, verdict bool, concluded time.Time, every time.Duration, why string) {
	d.diskMaintSchedule.record(name, concluded)
	key := diskMaintFailedKey(name)
	if verdict {
		d.cacheTrimClearAlert(ctx, key, label+" reached a verdict")
		return
	}
	if d.cacheTrimNoOp.alertOpen(key) {
		return
	}
	message := fmt.Sprintf("disk maintenance %s did not reach a verdict at %s: %s; the next attempt is at its normal cadence, in %s",
		label, concluded.Format(time.RFC3339), diskMaintReasonText(why), every)
	// The mark follows the send: a failed send leaves the item unmarked, so the
	// next failure raises it again.
	if err := d.escalateAlertSeverityCtx(ctx, cacheTrimAlertSeverity, key, "disk_maintenance", message); err != nil {
		d.cacheTrimLogf("disk_maintenance: %s: the alert was not sent: %v", name, err)
		return
	}
	d.cacheTrimNoOp.markAlertOpen(key)
}
