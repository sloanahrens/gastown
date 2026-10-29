package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/hmsync"
)

// The status fixtures below are what `hm status --json` answers with, trimmed
// to the fields the evaluator reads. They are not live captures: no test runs
// hm.
const (
	// hmSyncHealthStale is a status where every configured source is stale or
	// failing: the alert's condition.
	hmSyncHealthStale = `{"sources":[` +
		`{"source":"github","rig":"alpha","configured":true,"stale":true},` +
		`{"source":"gitlab","rig":"beta","configured":true,"failing":true}]}`

	// hmSyncHealthFresh is the same town with one source fresh again.
	hmSyncHealthFresh = `{"sources":[` +
		`{"source":"github","rig":"alpha","configured":true},` +
		`{"source":"gitlab","rig":"beta","configured":true,"stale":true}]}`
)

// hmSyncAlertCall is one raise the patrol asked for.
type hmSyncAlertCall struct {
	key     string
	source  string
	message string
}

// hmSyncClearCall is one clear the patrol asked for.
type hmSyncClearCall struct {
	reason string
	keys   []string
}

// hmSyncHealthFakes are the patrol's clock, the run seam's answers and the
// alert functions' record.
type hmSyncHealthFakes struct {
	now       time.Time
	syncs     int
	statuses  int
	statusCmd []string
	// statusOut and statusErr are what `hm status --json` answers.
	statusOut []byte
	statusErr error
	// syncErr is what `hm sync` returns.
	syncErr  error
	raised   []hmSyncAlertCall
	cleared  []hmSyncClearCall
	raiseErr error
	clearErr error
}

// hmSyncHealthTestDaemon builds a daemon whose patrol runs inline on the fake
// clock, with the run seam answering sync and status separately and the alert
// functions recorded rather than delivered.
func hmSyncHealthTestDaemon(t *testing.T) (*Daemon, *bytes.Buffer, *hmSyncHealthFakes) {
	t.Helper()
	f := &hmSyncHealthFakes{now: hmSyncNow}
	var buf bytes.Buffer
	d := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(&buf, "", 0),
		patrolConfig: hmSyncConfig(true, "30m", "hm"),
	}
	d.hmSync.now = func() time.Time { return f.now }
	d.hmSync.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == hmSyncStatusArg && args[1] == hmSyncStatusJSON {
			f.statuses++
			f.statusCmd = append([]string{name}, args...)
			return f.statusOut, f.statusErr
		}
		f.syncs++
		return nil, f.syncErr
	}
	// A sync and its health check run inline: a test asserts what they did, so
	// they must have done it by the time runHMSync returns.
	d.hmSync.dispatch = func(fn func()) { fn() }
	d.hmSyncAlerts.raise = func(key, source, message string) error {
		f.raised = append(f.raised, hmSyncAlertCall{key: key, source: source, message: message})
		return f.raiseErr
	}
	d.hmSyncAlerts.clearFn = func(reason string, keys ...string) error {
		f.cleared = append(f.cleared, hmSyncClearCall{reason: reason, keys: keys})
		return f.clearErr
	}
	return d, &buf, f
}

// hmSyncHealthNextInterval moves the clock past the interval, so the next tick
// runs the sync and its health check again.
func hmSyncHealthNextInterval(f *hmSyncHealthFakes) {
	f.now = f.now.Add(30 * time.Minute)
}

// TestHMSyncHealthRaisesOnceWhenEverySourceIsStale: a sync whose every
// configured source is stale or failing raises hmsync:stale once, from the
// patrol, naming the counts; the next interval's identical verdict changes
// nothing, so a source that stays down does not file an alert every interval.
func TestHMSyncHealthRaisesOnceWhenEverySourceIsStale(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncHealthTestDaemon(t)
	f.statusOut = []byte(hmSyncHealthStale)

	d.runHMSync()

	if len(f.raised) != 1 {
		t.Fatalf("raised %+v, want one alert", f.raised)
	}
	want := hmSyncAlertCall{
		key:     hmsync.AlertKey,
		source:  hmSyncAlertSource,
		message: "1 stale, 1 failing, 0 fresh of 2 configured hm sources",
	}
	if f.raised[0] != want {
		t.Errorf("raised %+v, want %+v", f.raised[0], want)
	}
	if len(f.cleared) != 0 {
		t.Errorf("cleared %+v, want no clear of an alert that was never open", f.cleared)
	}
	if f.statuses != 1 {
		t.Errorf("read the status %d times, want 1", f.statuses)
	}
	if got, wantCmd := strings.Join(f.statusCmd, " "), "hm status --json"; got != wantCmd {
		t.Errorf("status command = %q, want %q", got, wantCmd)
	}
	if got := logs.String(); !strings.Contains(got, "hm_sync: raised hmsync:stale: 1 stale, 1 failing") {
		t.Errorf("log = %q, want the raise named with what the status said", got)
	}

	hmSyncHealthNextInterval(f)
	d.runHMSync()

	if len(f.raised) != 1 {
		t.Errorf("raised %d times for one condition, want once: %+v", len(f.raised), f.raised)
	}
	if f.statuses != 2 {
		t.Errorf("read the status %d times over two intervals, want 2", f.statuses)
	}
}

// TestHMSyncHealthRaisesWhenNoSourceIsConfigured: a town whose status names no
// configured source is receiving no updates at all, so it raises.
func TestHMSyncHealthRaisesWhenNoSourceIsConfigured(t *testing.T) {
	t.Parallel()
	d, _, f := hmSyncHealthTestDaemon(t)
	f.statusOut = []byte(`{"sources":[]}`)

	d.runHMSync()

	if len(f.raised) != 1 {
		t.Fatalf("raised %+v, want one alert", f.raised)
	}
	if want := "no configured hm source: nothing is syncing"; f.raised[0].message != want {
		t.Errorf("raised %q, want %q", f.raised[0].message, want)
	}
}

// TestHMSyncHealthClearsWhenASourceIsFresh: an open alert closes once the
// status shows a fresh configured source, under the key it was raised with,
// and a second healthy interval has nothing left to clear.
func TestHMSyncHealthClearsWhenASourceIsFresh(t *testing.T) {
	t.Parallel()
	d, _, f := hmSyncHealthTestDaemon(t)
	f.statusOut = []byte(hmSyncHealthStale)

	d.runHMSync()
	if len(f.raised) != 1 {
		t.Fatalf("raised %+v, want the alert open before the clear", f.raised)
	}

	f.statusOut = []byte(hmSyncHealthFresh)
	hmSyncHealthNextInterval(f)
	d.runHMSync()

	if len(f.cleared) != 1 {
		t.Fatalf("cleared %+v, want one clear", f.cleared)
	}
	if got := f.cleared[0]; got.reason != hmSyncHealthyReason || len(got.keys) != 1 || got.keys[0] != hmsync.AlertKey {
		t.Errorf("cleared %+v, want reason %q for %q", got, hmSyncHealthyReason, hmsync.AlertKey)
	}

	hmSyncHealthNextInterval(f)
	d.runHMSync()
	if len(f.cleared) != 1 {
		t.Errorf("cleared %d times, want once: the second healthy interval has nothing to clear", len(f.cleared))
	}
}

// TestHMSyncHealthStaysQuietWhileFresh: a fresh town the patrol knows has no
// alert open raises and clears nothing, on every interval.
func TestHMSyncHealthStaysQuietWhileFresh(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncHealthTestDaemon(t)
	d.hmSyncAlerts.mark(hmSyncAlertClosed)
	f.statusOut = []byte(hmSyncHealthFresh)

	d.runHMSync()
	hmSyncHealthNextInterval(f)
	d.runHMSync()

	if len(f.raised) != 0 || len(f.cleared) != 0 {
		t.Errorf("raised %+v, cleared %+v, want neither for a fresh town", f.raised, f.cleared)
	}
	if f.statuses != 2 {
		t.Errorf("read the status %d times over two intervals, want 2", f.statuses)
	}
	if got := logs.String(); strings.Contains(got, hmsync.AlertKey) {
		t.Errorf("log = %q, want no alert line while the sources are fresh", got)
	}
}

// TestHMSyncHealthUnreadableStatusRaises: a status the patrol cannot read is
// not a healthy one. The command failing and the output not parsing both raise
// the same alert, with the unreadable detail, once.
func TestHMSyncHealthUnreadableStatusRaises(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  []byte
		err  error
		want string
	}{
		{
			name: "the command fails",
			err:  errors.New("hm: exit status 2"),
			want: "hm status unreadable: hm: exit status 2",
		},
		{
			name: "the output does not parse",
			out:  []byte(`{"sources":[`),
			want: "hm status unreadable: hm status json:",
		},
		{
			name: "the output has no sources key",
			out:  []byte(`{"generated_at":"2026-10-29T19:00:00Z"}`),
			want: "hm status unreadable: hm status json has no sources key",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, f := hmSyncHealthTestDaemon(t)
			f.statusOut, f.statusErr = tt.out, tt.err

			d.runHMSync()

			if len(f.raised) != 1 {
				t.Fatalf("raised %+v, want one alert", f.raised)
			}
			got := f.raised[0]
			if got.key != hmsync.AlertKey || got.source != hmSyncAlertSource {
				t.Errorf("raised %+v, want %q from %q", got, hmsync.AlertKey, hmSyncAlertSource)
			}
			if !strings.HasPrefix(got.message, tt.want) {
				t.Errorf("raised %q, want the detail starting %q", got.message, tt.want)
			}
			if len(f.cleared) != 0 {
				t.Errorf("cleared %+v, want no clear for an unreadable status", f.cleared)
			}

			// The alert is open, so an unreadable status read again changes
			// nothing; it is still reported, since the alert's detail is now an
			// older reading.
			hmSyncHealthNextInterval(f)
			d.runHMSync()
			if len(f.raised) != 1 {
				t.Errorf("raised %d times for one unreadable status, want once", len(f.raised))
			}
			if !strings.Contains(logs.String(), "hmsync:stale stays open: hm status unreadable:") {
				t.Errorf("log = %q, want the still-open alert named with the unreadable reason", logs.String())
			}
		})
	}
}

// TestHMSyncHealthClearsAnUnreadableAlertOnceTheStatusIsFresh: the alert an
// unreadable status raised closes the same way once the status is readable and
// every source it names is fresh.
func TestHMSyncHealthClearsAnUnreadableAlertOnceTheStatusIsFresh(t *testing.T) {
	t.Parallel()
	d, _, f := hmSyncHealthTestDaemon(t)
	f.statusErr = errors.New("hm: exit status 2")

	d.runHMSync()
	if len(f.raised) != 1 {
		t.Fatalf("raised %+v, want the unreadable alert open", f.raised)
	}

	f.statusErr = nil
	f.statusOut = []byte(hmSyncHealthFresh)
	hmSyncHealthNextInterval(f)
	d.runHMSync()

	if len(f.cleared) != 1 {
		t.Fatalf("cleared %+v, want one clear", f.cleared)
	}
	if got := f.cleared[0]; len(got.keys) != 1 || got.keys[0] != hmsync.AlertKey {
		t.Errorf("cleared %+v, want %q", got, hmsync.AlertKey)
	}
}

// TestHMSyncHealthChecksAfterAFailedSync: a sync that failed is still followed
// by the status read, and a stale town raises through it. A failure is not a
// reason to skip the alarm.
func TestHMSyncHealthChecksAfterAFailedSync(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncHealthTestDaemon(t)
	f.syncErr = exitErr(3)
	f.statusOut = []byte(hmSyncHealthStale)

	d.runHMSync()

	if f.syncs != 1 {
		t.Fatalf("ran %d syncs, want 1", f.syncs)
	}
	if f.statuses != 1 {
		t.Fatalf("read the status %d times after a failed sync, want 1", f.statuses)
	}
	if len(f.raised) != 1 {
		t.Errorf("raised %+v, want the alert a failed sync makes likelier", f.raised)
	}
	if got := logs.String(); !strings.Contains(got, "exit 3") {
		t.Errorf("log = %q, want the failed sync's summary line as well", got)
	}
}

// TestHMSyncHealthReadsTheConfiguredCommand: the status read uses the town's
// hm command, not a name of its own.
func TestHMSyncHealthReadsTheConfiguredCommand(t *testing.T) {
	t.Parallel()
	d, _, f := hmSyncHealthTestDaemon(t)
	d.patrolConfig = hmSyncConfig(true, "30m", "/opt/hm/bin/hm")
	f.statusOut = []byte(hmSyncHealthFresh)

	d.runHMSync()

	if got, want := strings.Join(f.statusCmd, " "), "/opt/hm/bin/hm status --json"; got != want {
		t.Errorf("status command = %q, want %q", got, want)
	}
}

// TestHMSyncHealthUndeliveredRaiseIsRaisedAgain: an alert the notifier refused
// is not recorded as open, so the next interval raises it again instead of
// leaving the condition silently unannounced.
func TestHMSyncHealthUndeliveredRaiseIsRaisedAgain(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncHealthTestDaemon(t)
	f.statusOut = []byte(hmSyncHealthStale)
	f.raiseErr = errors.New("gt escalate: exit status 1")

	d.runHMSync()
	if len(f.raised) != 1 {
		t.Fatalf("raised %+v, want one attempt", f.raised)
	}
	if !strings.Contains(logs.String(), "hm_sync: alert hmsync:stale was not delivered") {
		t.Errorf("log = %q, want the undelivered alert named", logs.String())
	}

	f.raiseErr = nil
	hmSyncHealthNextInterval(f)
	d.runHMSync()
	if len(f.raised) != 2 {
		t.Fatalf("raised %d times, want the retry after the refused delivery", len(f.raised))
	}

	// The delivered raise is remembered, so a third interval is quiet.
	hmSyncHealthNextInterval(f)
	d.runHMSync()
	if len(f.raised) != 2 {
		t.Errorf("raised %d times, want the delivered alert raised once", len(f.raised))
	}
}

// TestHMSyncHealthUndeliveredClearKeepsTheAlertOpen: a clear the notifier
// refused leaves the alert open, so the next healthy interval clears again.
func TestHMSyncHealthUndeliveredClearKeepsTheAlertOpen(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncHealthTestDaemon(t)
	f.statusOut = []byte(hmSyncHealthStale)
	d.runHMSync()
	if len(f.raised) != 1 {
		t.Fatalf("raised %+v, want the alert open before the clear", f.raised)
	}

	f.statusOut = []byte(hmSyncHealthFresh)
	f.clearErr = errors.New("gt escalate clear: exit status 1")
	hmSyncHealthNextInterval(f)
	d.runHMSync()
	if len(f.cleared) != 1 {
		t.Fatalf("cleared %+v, want one attempt", f.cleared)
	}
	if !strings.Contains(logs.String(), "hm_sync: alert hmsync:stale was not cleared") {
		t.Errorf("log = %q, want the refused clear named", logs.String())
	}

	f.clearErr = nil
	hmSyncHealthNextInterval(f)
	d.runHMSync()
	if len(f.cleared) != 2 {
		t.Fatalf("cleared %d times, want the retry after the refused clear", len(f.cleared))
	}

	// The clear landed, so a third healthy interval has nothing to do.
	hmSyncHealthNextInterval(f)
	d.runHMSync()
	if len(f.cleared) != 2 {
		t.Errorf("cleared %d times, want the alert cleared once", len(f.cleared))
	}
}

// TestHMSyncHealthHealthyAfterRestartClearsOnce: a restart forgets the open
// flag, so the first healthy read clears the alert an earlier run may have
// left open, and a later healthy read has nothing to clear.
func TestHMSyncHealthHealthyAfterRestartClearsOnce(t *testing.T) {
	t.Parallel()
	d, _, f := hmSyncHealthTestDaemon(t)
	f.statusOut = []byte(hmSyncHealthFresh)

	d.runHMSync()
	if len(f.cleared) != 1 {
		t.Fatalf("cleared %+v, want the first healthy read after the boot to clear", f.cleared)
	}
	if got := f.cleared[0]; len(got.keys) != 1 || got.keys[0] != hmsync.AlertKey {
		t.Errorf("cleared %+v, want %q", got, hmsync.AlertKey)
	}

	hmSyncHealthNextInterval(f)
	d.runHMSync()
	if len(f.cleared) != 1 {
		t.Errorf("cleared %d times, want once: the alert is known closed after the first clear", len(f.cleared))
	}
}

// TestHMSyncHealthShutdownRaisesNothing: a status read cut short by the daemon
// stopping is not an unreadable status, so it raises nothing and leaves the
// alert as it was.
func TestHMSyncHealthShutdownRaisesNothing(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncHealthTestDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.ctx = ctx
	f.statusErr = context.Canceled

	d.checkHMSyncHealth()

	if len(f.raised) != 0 || len(f.cleared) != 0 {
		t.Errorf("raised %+v, cleared %+v, want neither while the daemon shuts down", f.raised, f.cleared)
	}
	if got := logs.String(); strings.Contains(got, "unreadable") {
		t.Errorf("log = %q, want no unreadable line for a read cut short by shutdown", got)
	}
}
