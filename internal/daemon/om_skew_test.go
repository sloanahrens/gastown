package daemon

import (
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

const (
	omSkewInstalledSHA = "1111111111111111111111111111111111111111"
	omSkewRemoteSHA    = "2222222222222222222222222222222222222222"
)

// omSkewDaemon is a Daemon with the landing worker patrol on and om review on
// (the condition the check runs under), both readings injected, and everything
// it escalates recorded. Nothing it does starts a process or reaches a remote.
func omSkewDaemon(t *testing.T) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	rec := notifyfake.New()
	review := true
	d := &Daemon{
		logger:   discardLogger,
		config:   &Config{TownRoot: t.TempDir()},
		notifier: rec,
		clock:    newFixedClock(),
		ctx:      t.Context(),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			LandingWorker: &LandingWorkerConfig{Enabled: true, Review: &review},
		}},
	}
	d.omVersionFn = func(string) (string, error) { return omSkewInstalledSHA, nil }
	d.omHeadFn = func(string) (string, error) { return omSkewRemoteSHA, nil }
	return d, rec
}

// TestOMSkewMismatchRaisesOnceThenClears: a skew raises one keyed alert naming
// both shas, a second check of the same skew adds nothing, and the alert
// clears when the installed om reaches origin/main (gt-pdyzk acceptance 1, 2).
func TestOMSkewMismatchRaisesOnceThenClears(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)

	d.omSkewOnce() // the start check
	d.omSkewOnce() // the next hour, the same skew

	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want exactly one for a skew that has not changed", esc)
	}
	got := esc[0].Escalation
	if got.Fingerprint != omSkewAlertKey {
		t.Errorf("fingerprint = %q, want %q", got.Fingerprint, omSkewAlertKey)
	}
	if got.Severity != "MEDIUM" {
		t.Errorf("severity = %q, want the informing MEDIUM the rebuild_gt alerts use", got.Severity)
	}
	for _, want := range []string{omSkewInstalledSHA[:8], omSkewRemoteSHA[:8], omSkewSource} {
		if !strings.Contains(got.Reason+" "+got.Description, want) {
			t.Errorf("alert %q / %q does not name %q", got.Description, got.Reason, want)
		}
	}

	d.omHeadFn = func(string) (string, error) { return omSkewInstalledSHA, nil }
	d.omSkewOnce()
	clears := rec.Clears()
	if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, omSkewAlertKey) {
		t.Fatalf("clears = %+v, want one under %s once the shas match", clears, omSkewAlertKey)
	}
	d.omSkewOnce()
	if len(rec.Clears()) != 1 {
		t.Errorf("clears = %+v, want none for a condition that was already closed", rec.Clears())
	}
}

// TestOMSkewEqualReadsLeaveNoAlert: an om in step with origin/main raises
// nothing and clears nothing (gt-pdyzk acceptance 1).
func TestOMSkewEqualReadsLeaveNoAlert(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)
	d.omHeadFn = func(string) (string, error) { return omSkewInstalledSHA, nil }

	d.omSkewOnce()
	d.omSkewOnce()

	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("escalations = %+v, want none for an om at origin/main", esc)
	}
	if clears := rec.Clears(); len(clears) != 0 {
		t.Errorf("clears = %+v, want none for an alert that never fired", clears)
	}
}

// TestOMSkewUnreadableReadingChangesNothingAndLogs: an om that cannot be
// versioned and a remote that cannot be reached are neither a skew nor the end
// of one. Each leaves the alert state alone and is logged once per change
// (gt-pdyzk acceptance 3).
func TestOMSkewUnreadableReadingChangesNothingAndLogs(t *testing.T) {
	t.Parallel()

	t.Run("an unreadable version raises and clears nothing", func(t *testing.T) {
		t.Parallel()
		d, rec := omSkewDaemon(t)
		var logs lockedBuffer
		d.logger = log.New(&logs, "", 0)
		d.omVersionFn = func(string) (string, error) {
			return "", errors.New("exec: \"om\": executable file not found in $PATH")
		}

		d.omSkewOnce()
		d.omSkewOnce()

		if esc := rec.Escalations(); len(esc) != 0 {
			t.Errorf("escalations = %+v, want none for a version that could not be read", esc)
		}
		if clears := rec.Clears(); len(clears) != 0 {
			t.Errorf("clears = %+v, want none for a version that could not be read", clears)
		}
		if got := strings.Count(logs.String(), "executable file not found"); got != 1 {
			t.Errorf("logged the same unreadable version %d times, want once: %s", got, logs.String())
		}
	})

	t.Run("an unreachable remote raises and clears nothing", func(t *testing.T) {
		t.Parallel()
		d, rec := omSkewDaemon(t)
		var logs lockedBuffer
		d.logger = log.New(&logs, "", 0)
		d.omHeadFn = func(string) (string, error) { return "", errors.New("ls-remote: could not resolve host") }

		d.omSkewOnce()

		if esc := rec.Escalations(); len(esc) != 0 {
			t.Errorf("escalations = %+v, want none for a remote that could not be read", esc)
		}
		if clears := rec.Clears(); len(clears) != 0 {
			t.Errorf("clears = %+v, want none for a remote that could not be read", clears)
		}
		if !strings.Contains(logs.String(), "could not resolve host") {
			t.Errorf("the unreadable remote was not logged: %s", logs.String())
		}
	})

	t.Run("an open skew survives an unreadable reading", func(t *testing.T) {
		t.Parallel()
		d, rec := omSkewDaemon(t)

		d.omSkewOnce()
		d.omVersionFn = func(string) (string, error) { return "", errors.New("om is a directory") }
		d.omSkewOnce()

		if esc := rec.Escalations(); len(esc) != 1 {
			t.Errorf("escalations = %+v, want the one open alert left alone", esc)
		}
		if clears := rec.Clears(); len(clears) != 0 {
			t.Errorf("clears = %+v, want none: an unreadable om is not a cleared skew", clears)
		}

		d.omVersionFn = func(string) (string, error) { return omSkewInstalledSHA, nil }
		d.omHeadFn = func(string) (string, error) { return omSkewInstalledSHA, nil }
		d.omSkewOnce()
		if clears := rec.Clears(); len(clears) != 1 {
			t.Errorf("clears = %+v, want the equal reading after it to close the alert", clears)
		}
	})
}

// TestOMSkewDroppedEscalationRetries: an escalation that never reached the
// town leaves the state unraised, so the next check raises it again. An alarm
// that records itself as raised after a drop is worse than no alarm.
func TestOMSkewDroppedEscalationRetries(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)
	clk := newFixedClock()
	d.clock = clk
	rec.Fail(notifyfake.KindEscalate, errors.New("gt escalate: Dolt is unreachable"))

	// Each failed attempt backs off on the daemon's clock (1s, then 2s), so
	// the drop is driven rather than waited out.
	runOnClock(t, clk, time.Second, d.omSkewOnce)

	rec.Fail(notifyfake.KindEscalate, nil)
	d.omSkewOnce()

	got := rec.Escalations()
	if len(got) != maxEscalationRetries+1 {
		t.Fatalf("escalations = %d, want the %d failed attempts and the retry on the next check", len(got), maxEscalationRetries)
	}
	if got[len(got)-1].Err != nil {
		t.Errorf("the retry after the drop failed too: %v", got[len(got)-1].Err)
	}
}

// TestOMSkewStandsDownWithItsPatrol: the check is the landing worker's, so a
// daemon whose landing_worker patrol is off, or whose landing review is off,
// never reads om at all.
func TestOMSkewStandsDownWithItsPatrol(t *testing.T) {
	t.Parallel()
	off := false
	cases := map[string]*DaemonPatrolConfig{
		"patrol off": {Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Enabled: false}}},
		"review off": {Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Enabled: true, Review: &off}}},
		"no config":  {Patrols: &PatrolsConfig{}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, rec := omSkewDaemon(t)
			d.patrolConfig = cfg
			var mu sync.Mutex
			reads := 0
			d.omVersionFn = func(string) (string, error) {
				mu.Lock()
				reads++
				mu.Unlock()
				return omSkewInstalledSHA, nil
			}

			d.omSkewOnce()

			if reads != 0 {
				t.Errorf("the installed om was read %d times with no landing review to serve", reads)
			}
			if esc := rec.Escalations(); len(esc) != 0 {
				t.Errorf("escalations = %+v, want none", esc)
			}
		})
	}
}

// TestOMSkewMonitorChecksAtStartAndThenHourly: the first check runs as the
// monitor starts and one more per hour on the daemon's clock, no more often
// (gt-pdyzk acceptance 1, 4).
func TestOMSkewMonitorChecksAtStartAndThenHourly(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)
	clk := newFixedClock()
	d.clock = clk
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.ctx = ctx

	// One signal per check, sent as the check takes its remote reading: the
	// clock is advanced only after a reading, so one tick buys exactly one
	// check and the ticker never holds a spare tick to spend at the end.
	reads := make(chan struct{}, 8)
	d.omHeadFn = func(string) (string, error) {
		reads <- struct{}{}
		return omSkewRemoteSHA, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.runOMSkewMonitor()
	}()
	nextCheck := func(what string) {
		t.Helper()
		waitForSignal(t, reads, "the monitor took no reading "+what)
	}

	nextCheck("at start")
	clk.BlockUntil(1) // the ticker is armed before the clock moves
	clk.Advance(omSkewInterval)
	nextCheck("an hour in")
	clk.Advance(omSkewInterval)
	nextCheck("two hours in")
	cancel()
	<-done

	select {
	case <-reads:
		t.Error("the monitor checked without an hour having passed")
	default:
	}
	if esc := rec.Escalations(); len(esc) != 1 {
		t.Errorf("escalations = %+v, want one for one unchanged skew", esc)
	}
}

// TestOMSkewMonitorRunsOffTheStartersGoroutine: starting the monitor returns
// without waiting on a reading, so neither the landing worker manager's loop
// nor a landing pass can be parked behind a hung `om version` or ls-remote
// (gt-pdyzk acceptance 4).
func TestOMSkewMonitorRunsOffTheStartersGoroutine(t *testing.T) {
	t.Parallel()
	d, _ := omSkewDaemon(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	d.omVersionFn = func(string) (string, error) {
		started <- struct{}{}
		<-release
		return omSkewInstalledSHA, nil
	}

	returned := make(chan struct{})
	go func() {
		d.startOMSkewMonitor()
		close(returned)
	}()
	waitForSignal(t, returned, "starting the monitor did not return")
	waitForSignal(t, started, "the monitor never ran its start check")
	close(release)
}

// TestOMSkewMonitorStartsOnce: the manager calls the starter on every pass,
// and one daemon runs one monitor.
func TestOMSkewMonitorStartsOnce(t *testing.T) {
	t.Parallel()
	d, _ := omSkewDaemon(t)
	reads := make(chan struct{}, 8)
	d.omHeadFn = func(string) (string, error) {
		reads <- struct{}{}
		return omSkewRemoteSHA, nil
	}

	for range 3 {
		d.startOMSkewMonitor()
	}
	waitForSignal(t, reads, "the monitor never ran its start check")

	// Only one monitor can exist, and its next check is an hour away on the
	// clock this test never advances, so a second reading here is a second
	// monitor let through.
	select {
	case <-reads:
		t.Error("three starts ran more than one monitor")
	default:
	}
}

// waitForSignal blocks until ch delivers, or the wait deadline passes: a
// check or a start that never happens fails the test rather than hanging it.
// The bound is a context, not time.After, which unit tests may not use.
func waitForSignal(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(failure)
	}
}

// TestParseOMVersion pins the reading of the installed side: the sha is what
// follows "om version " (gt-pdyzk).
func TestParseOMVersion(t *testing.T) {
	t.Parallel()
	const sha = "2d8164fd8dcdd5cbaf52315014e89331a83c4454"
	cases := map[string]struct {
		out string
		sha string
		ok  bool
	}{
		"the sha line alone":      {out: "om version " + sha + "\n", sha: sha, ok: true},
		"the sha line alone bare": {out: "om version " + sha, sha: sha, ok: true},
		"trailing text":           {out: "om version " + sha + " (2026-10-01)\n", sha: sha, ok: true},
		"among other lines":       {out: "om build linux/arm64\nom version " + sha + "\n", sha: sha, ok: true},
		"no version line":         {out: "om build linux/arm64\n", ok: false},
		"no sha":                  {out: "om version \n", ok: false},
		"a version that is a tag": {out: "om version v1.2.3\n", ok: false},
		"empty":                   {out: "", ok: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := parseOMVersion(tc.out)
			if tc.ok && (err != nil || got != tc.sha) {
				t.Errorf("parseOMVersion(%q) = %q, %v; want %q", tc.out, got, err, tc.sha)
			}
			if !tc.ok && err == nil {
				t.Errorf("parseOMVersion(%q) = %q, want an error", tc.out, got)
			}
		})
	}
}

// TestParseLSRemoteMain pins the reading of the remote side: the ref is
// matched whole, so a branch that merely starts with it is not the tip.
func TestParseLSRemoteMain(t *testing.T) {
	t.Parallel()
	const sha = "abcdef1234567890abcdef1234567890abcdef12"
	cases := map[string]struct {
		out string
		sha string
	}{
		"the main tip":         {out: sha + "\trefs/heads/main\n", sha: sha},
		"with other refs":      {out: "aaa\trefs/heads/dev\n" + sha + "\trefs/heads/main\n", sha: sha},
		"a prefixed branch":    {out: sha + "\trefs/heads/main-old\n", sha: ""},
		"a tag":                {out: sha + "\trefs/tags/v1\n", sha: ""},
		"an empty answer":      {out: "", sha: ""},
		"a malformed line":     {out: "not-a-sha refs/heads/main\nrefs/heads/main\n", sha: ""},
		"an empty-tip answer":  {out: "\n", sha: ""},
		"a head without a tab": {out: sha + " refs/heads/main", sha: sha},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := parseLSRemoteMain(tc.out); got != tc.sha {
				t.Errorf("parseLSRemoteMain(%q) = %q, want %q", tc.out, got, tc.sha)
			}
		})
	}
}
