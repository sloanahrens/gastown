package daemon

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestDiskMaintConcludeFailureRaisesOnceAndSchedulesTheFullInterval pins
// gt-q9gvmy: a failed conclusion records the full interval, leaves no retry
// time, and raises one item naming the label, the time and the why; a second
// consecutive failure raises nothing more.
func TestDiskMaintConcludeFailureRaisesOnceAndSchedulesTheFullInterval(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	const name, label, why = "ci-cache-trim/vol", "the CI cache volume trim of vol", "kubectl exited 1"
	every := 2 * time.Hour

	d.diskMaintConclude(context.Background(), name, label, false, diskMaintNow, every, why)

	if got := d.diskMaintSchedule.lastRunAt(name); !got.Equal(diskMaintNow) {
		t.Errorf("last run = %v, want the failed conclusion %v", got, diskMaintNow)
	}
	if got := d.diskMaintSchedule.retryAtOf(name); !got.IsZero() {
		t.Errorf("retry at = %v, want none: a failure asks for no retry", got)
	}
	open := f.notify.OpenEscalations(diskMaintFailedKey(name))
	if len(open) != 1 {
		t.Fatalf("open items = %+v, want one", open)
	}
	if !strings.Contains(open[0].Reason, label) || !strings.Contains(open[0].Reason, why) ||
		!strings.Contains(open[0].Reason, diskMaintNow.Format(time.RFC3339)) {
		t.Errorf("the item does not name the label, time and why: %q", open[0].Reason)
	}

	later := diskMaintNow.Add(every)
	d.diskMaintConclude(context.Background(), name, label, false, later, every, why)
	if got := len(f.notify.Escalations()); got != 1 {
		t.Errorf("escalations = %d after a second failure, want the one already open", got)
	}
}

// TestDiskMaintConcludeSuccessClearsAndASecondFailureRaisesAgain pins the
// other half: a conclusion that reaches a verdict clears the item a failure
// opened, and a failure after it raises a new one.
func TestDiskMaintConcludeSuccessClearsAndASecondFailureRaisesAgain(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	const name, label, why = "ci-cache-trim/vol", "the CI cache volume trim of vol", "kubectl exited 1"
	key := diskMaintFailedKey(name)
	every := 2 * time.Hour

	d.diskMaintConclude(context.Background(), name, label, false, diskMaintNow, every, why)
	if open := f.notify.OpenEscalations(key); len(open) != 1 {
		t.Fatalf("open items after the failure = %+v, want one", open)
	}

	cleared := diskMaintNow.Add(every)
	d.diskMaintConclude(context.Background(), name, label, true, cleared, every, "")
	if open := f.notify.OpenEscalations(key); len(open) != 0 {
		t.Errorf("open items after the success = %+v, want none", open)
	}
	if got := d.diskMaintSchedule.lastRunAt(name); !got.Equal(cleared) {
		t.Errorf("last run after the success = %v, want %v", got, cleared)
	}

	failedAgain := cleared.Add(every)
	d.diskMaintConclude(context.Background(), name, label, false, failedAgain, every, why)
	if open := f.notify.OpenEscalations(key); len(open) != 1 {
		t.Errorf("open items after a later failure = %+v, want one", open)
	}
}

// TestDiskMaintConcludeFailedSendIsNotMarkedOpen pins the send's end: a
// failure whose alert did not land is logged and left unmarked, so the next
// failure raises it rather than reading it as already open.
func TestDiskMaintConcludeFailedSendIsNotMarkedOpen(t *testing.T) {
	t.Parallel()
	d, buf, f := diskMaintTestDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := diskMaintFailedKey("vol")

	d.diskMaintConclude(ctx, "vol", "the trim", false, diskMaintNow, time.Hour, "why")

	if open := f.notify.OpenEscalations(key); len(open) != 0 {
		t.Errorf("a failed send left the item open: %+v", open)
	}
	if !strings.Contains(buf.String(), "was not sent") {
		t.Errorf("the log does not say the alert was not sent: %s", buf.String())
	}
	d.diskMaintConclude(context.Background(), "vol", "the trim", false, diskMaintNow, time.Hour, "why")
	if open := f.notify.OpenEscalations(key); len(open) != 1 {
		t.Errorf("the next conclusion did not raise the item: %+v", open)
	}
}
