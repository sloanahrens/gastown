package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// diskMaintWantMessage is the item text a failure concluding at diskMaintNow
// files, byte for byte, so a reason test pins the whole string.
func diskMaintWantMessage(label, why string, every time.Duration) string {
	return fmt.Sprintf("disk maintenance %s did not reach a verdict at %s: %s; the next attempt is at its normal cadence, in %s",
		label, diskMaintNow.Format(time.RFC3339), why, every)
}

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

// TestDiskMaintConcludeReasonFirstLineAndCut pins gt-u6garf: the failure item
// carries the patrol's own reason, its first line with carriage returns and
// tabs as single spaces and trimmed, and a reason longer than 300 runes is cut
// to 300 with "..." as the last three.
func TestDiskMaintConcludeReasonFirstLineAndCut(t *testing.T) {
	t.Parallel()
	const name, label = "ci-cache-trim/vol", "the CI cache volume trim of vol"
	every := 2 * time.Hour

	cases := []struct {
		name string
		why  string
		want string
	}{
		{
			name: "a multi-line reason keeps only its first line",
			why:  "kubectl exited 1\nthe pod refused the volume",
			want: "kubectl exited 1",
		},
		{
			name: "carriage return and tab become single spaces",
			why:  "kubectl\texited\t1\r\nsecond line",
			want: "kubectl exited 1",
		},
		{
			name: "a reason longer than 300 runes is cut to 300 with ... last",
			why:  strings.Repeat("x", 301),
			want: strings.Repeat("x", 297) + "...",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, f := diskMaintTestDaemon(t)
			d.diskMaintConclude(context.Background(), name, label, false, diskMaintNow, every, tt.why)
			open := f.notify.OpenEscalations(diskMaintFailedKey(name))
			if len(open) != 1 {
				t.Fatalf("open items = %+v, want one", open)
			}
			if want := diskMaintWantMessage(label, tt.want, every); open[0].Reason != want {
				t.Errorf("item text = %q, want %q", open[0].Reason, want)
			}
		})
	}
}

// TestDiskMaintConcludeReasonEmptyKeepsTheFixedSentence pins the other half of
// gt-u6garf: an empty reason and a white-space-only reason raise an item whose
// text holds the fixed sentence, byte for byte.
func TestDiskMaintConcludeReasonEmptyKeepsTheFixedSentence(t *testing.T) {
	t.Parallel()
	const name, label = "ci-cache-trim/vol", "the CI cache volume trim of vol"
	every := 2 * time.Hour

	cases := []struct {
		name string
		why  string
	}{
		{"an empty reason", ""},
		{"a white-space-only reason", " \t\n\r  "},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, f := diskMaintTestDaemon(t)
			d.diskMaintConclude(context.Background(), name, label, false, diskMaintNow, every, tt.why)
			open := f.notify.OpenEscalations(diskMaintFailedKey(name))
			if len(open) != 1 {
				t.Fatalf("open items = %+v, want one", open)
			}
			if want := diskMaintWantMessage(label, diskMaintNoReason, every); open[0].Reason != want {
				t.Errorf("item text = %q, want %q", open[0].Reason, want)
			}
		})
	}
}
