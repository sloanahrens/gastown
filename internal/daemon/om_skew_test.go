package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
	d.omSkew.refresh = func(string) (string, error) { return "", nil } // no shared fake answers its git
	return d, rec
}

// skewBareRunner answers a skew probe's git calls the way git answers a
// --git-dir: `remote get-url origin` prints url while the named repository
// exists and exits 128 with git's fatal line while it does not, and ls-remote
// prints origin's main tip. Every call's argv is recorded for the test to read.
type skewBareRunner struct {
	mu    sync.Mutex
	url   string
	calls [][]string
}

func (r *skewBareRunner) run(cmd *exec.Cmd) ([]byte, []byte, error) {
	args := slices.Clone(cmd.Args)
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.mu.Unlock()

	var out string
	var err error
	switch {
	case slices.Contains(args, "remote"):
		gitDir := ""
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--git-dir="); ok {
				gitDir = v
			}
		}
		if _, statErr := os.Stat(gitDir); statErr != nil {
			out = "fatal: not a git repository: '" + gitDir + "'\n"
			err = &cliExitError{code: 128}
		} else {
			out = r.url + "\n"
		}
	case slices.Contains(args, "ls-remote"):
		out = omSkewRemoteSHA + "\t" + omSkewRemoteRef + "\n"
	default:
		err = errors.New("unexpected call: " + strings.Join(args, " "))
	}
	if _, werr := io.WriteString(cmd.Stdout, out); werr != nil {
		return nil, nil, werr
	}
	return nil, nil, err
}

// calledWithMayorRig reports whether any recorded call's argv names a path
// under a mayor/rig clone, the checkout the probes must no longer read.
func (r *skewBareRunner) calledWithMayorRig() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, args := range r.calls {
		for _, a := range args {
			if strings.Contains(a, "mayor/rig") {
				return true
			}
		}
	}
	return false
}

// TestOMSkewAndHMSkewRemoteReadsTheBareRepo: the om and hm rigs' origin urls are
// read from <town>/<rig>/.repo.git and never from the mayor/rig clone, the tip
// is read from that url, and a rig with no bare repo fails naming the bare path
// rather than falling back to a clone. One table covers both probes.
func TestOMSkewAndHMSkewRemoteReadsTheBareRepo(t *testing.T) {
	t.Parallel()
	omProbe := func(d *Daemon, town string) (string, error) {
		return d.omSkewRemoteSHA(town)
	}
	hmProbe := func(d *Daemon, town string) (string, error) {
		_, sha, err := d.hmSkewRemote(town)
		return sha, err
	}
	const omURL = "https://example.invalid/om.git"
	tests := []struct {
		name    string
		rig     string // the rig whose bare repo the probe reads
		probe   func(*Daemon, string) (string, error)
		bare    bool   // the rig's .repo.git exists
		url     string // the origin url the bare repo prints
		wantErr bool
		// wantLSRemote, when set, is the exact argv the ls-remote call must
		// carry: the url follows a "--", so one beginning with "-" is read as
		// a url and not an option.
		wantLSRemote []string
	}{
		{name: "om: the bare repo answers the url and the tip", rig: "om", probe: omProbe, bare: true, url: omURL},
		{name: "om: a url that begins with a dash is read after --", rig: "om", probe: omProbe, bare: true, url: "-oops",
			wantLSRemote: []string{"git", "ls-remote", "--", "-oops", omSkewRemoteRef}},
		{name: "om: a rig with no bare repo fails naming it", rig: "om", probe: omProbe, bare: false, url: omURL, wantErr: true},
		{name: "om: a bare repo with no origin url fails naming it", rig: "om", probe: omProbe, bare: true, url: "", wantErr: true},
		// hm's failure rows (no origin url, a failed ls-remote) are the hm_skew
		// tests' own; here hm proves only that it reads the bare repo and never
		// the mayor/rig clone.
		{name: "hm: the bare repo answers the url and the tip", rig: "hm", probe: hmProbe, bare: true, url: hmSkewTestURL},
		{name: "hm: a rig with no bare repo fails naming it", rig: "hm", probe: hmProbe, bare: false, url: hmSkewTestURL, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			bare := filepath.Join(town, tc.rig, ".repo.git")
			if tc.bare {
				mkdirs(t, bare)
			}
			r := &skewBareRunner{url: tc.url}
			d := &Daemon{config: &Config{TownRoot: town}, execCmd: r.run}

			sha, err := tc.probe(d, town)

			if r.calledWithMayorRig() {
				t.Errorf("a probe read the mayor/rig clone: calls %q", r.calls)
			}
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), bare) {
					t.Fatalf("probe error = %v, want one naming %s", err, bare)
				}
				if sha != "" {
					t.Errorf("sha = %q with no bare repo, want none", sha)
				}
				return
			}
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if sha != omSkewRemoteSHA {
				t.Errorf("sha = %q, want origin's main tip %q", sha, omSkewRemoteSHA)
			}
			want := []string{"git", "--git-dir=" + bare, "remote", "get-url", "origin"}
			if !slices.ContainsFunc(r.calls, func(c []string) bool { return slices.Equal(c, want) }) {
				t.Fatalf("git calls = %q, want one get-url call %q against the bare repo", r.calls, want)
			}
			if lsRemote := r.calls[len(r.calls)-1]; !slices.Contains(lsRemote, tc.url) {
				t.Errorf("ls-remote call = %q, want it against the bare repo's url %q", lsRemote, tc.url)
			}
			if tc.wantLSRemote != nil && !slices.ContainsFunc(r.calls, func(c []string) bool {
				return slices.Equal(c, tc.wantLSRemote)
			}) {
				t.Errorf("git calls = %q, want an ls-remote call %q", r.calls, tc.wantLSRemote)
			}
		})
	}
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

// TestOMSkewMessageNamesThePinRefresh: the escalation names the image pin the
// image-pins planner refreshes, not an install from the rig (gt-d216w1).
func TestOMSkewMessageNamesThePinRefresh(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)
	d.omSkewOnce()
	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want one for a skew", esc)
	}
	msg := esc[0].Escalation.Reason
	for _, want := range []string{omSkewInstalledSHA[:8], omSkewRemoteSHA[:8], "image pin", "image-pins-refresh"} {
		if !strings.Contains(msg, want) {
			t.Errorf("alert reason %q does not name %q", msg, want)
		}
	}
	if strings.Contains(msg, "install om from") {
		t.Errorf("alert reason %q still tells the operator to install om from the rig", msg)
	}
}

// TestOMSkewEqualReadsLeaveNoAlert: an om in step with origin/main raises
// nothing, and clears once, on the process's first reading, for an alert a
// previous process may have left open (gt-pdyzk acceptance 1, gt-rg2xc).
func TestOMSkewEqualReadsLeaveNoAlert(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)
	d.omHeadFn = func(string) (string, error) { return omSkewInstalledSHA, nil }

	d.omSkewOnce()
	d.omSkewOnce()
	d.omSkewOnce()

	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("escalations = %+v, want none for an om at origin/main", esc)
	}
	clears := rec.Clears()
	if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, omSkewAlertKey) {
		t.Errorf("clears = %+v, want exactly one under %s across three matching readings", clears, omSkewAlertKey)
	}
}

// TestOMSkewClearsAnAlertOpenAcrossARestart: the raised flag lives in the
// process, so a daemon that restarts with om-skew open starts unraised. Its
// first matching reading clears the key; it does not re-raise it while the
// skew persists (gt-rg2xc).
func TestOMSkewClearsAnAlertOpenAcrossARestart(t *testing.T) {
	t.Parallel()

	t.Run("a fresh judge clears on a matching reading", func(t *testing.T) {
		t.Parallel()
		d, rec := omSkewDaemon(t) // raised = false, as after a restart
		d.omHeadFn = func(string) (string, error) { return omSkewInstalledSHA, nil }

		d.omSkewOnce()

		clears := rec.Clears()
		if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, omSkewAlertKey) {
			t.Fatalf("clears = %+v, want the one open-across-restart alert cleared under %s", clears, omSkewAlertKey)
		}
		if esc := rec.Escalations(); len(esc) != 0 {
			t.Errorf("escalations = %+v, want none", esc)
		}
	})

	t.Run("a fresh judge does not re-raise an open skew", func(t *testing.T) {
		t.Parallel()
		d, rec := omSkewDaemon(t)
		d.omSkewOnce() // the skew persists; this process raises it itself
		d.omSkewOnce()

		if clears := rec.Clears(); len(clears) != 0 {
			t.Errorf("clears = %+v, want none while the skew persists", clears)
		}
		if esc := rec.Escalations(); len(esc) != 1 {
			t.Errorf("escalations = %+v, want the one raise", esc)
		}
	})

	t.Run("a clear that does not reach the town is retried next check", func(t *testing.T) {
		t.Parallel()
		d, rec := omSkewDaemon(t)
		d.omHeadFn = func(string) (string, error) { return omSkewInstalledSHA, nil }
		rec.Fail(notifyfake.KindClear, errors.New("gt escalate: Dolt is unreachable"))

		d.omSkewOnce()
		rec.Fail(notifyfake.KindClear, nil)
		d.omSkewOnce()
		d.omSkewOnce()

		clears := rec.Clears()
		if len(clears) != 2 {
			t.Fatalf("clears = %+v, want the dropped attempt and one retry, then quiet", clears)
		}
		if clears[1].Err != nil {
			t.Errorf("the retry failed too: %v", clears[1].Err)
		}
	})
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

// omSkewClone gives the daemon the om-skew check's own clone and answers its git
// diff with changed (NUL-separated, as `git diff -z` prints it) or, when
// diffErr is set, with that failure. Its lag reads put the installed om at the
// planner's commit threshold. It returns the fake so a test can read the calls
// the check made.
func omSkewClone(t *testing.T, d *Daemon, changed []string, diffErr *cliReply) *fakeCLI {
	t.Helper()
	mkdirs(t, filepath.Join(d.config.TownRoot, omSkewSrcDir, ".git"))
	return withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if c.name == "git" && slices.Contains(c.args, "diff") {
			if diffErr != nil {
				return *diffErr
			}
			return cliReply{stdout: strings.Join(changed, "\x00") + "\x00"}
		}
		if reply, ok := omSkewLagReply(c, imagePinsFileBehind, 0); ok {
			return reply
		}
		return cliReply{}
	})
}

// omSkewLagReply answers skewLag's git calls for an installed om that is
// commits commits and age behind the remote; ok is false for any other call.
func omSkewLagReply(c cliCall, commits int, age time.Duration) (cliReply, bool) {
	switch {
	case gitSub(c, "rev-list"):
		return cliReply{stdout: fmt.Sprintf("%d\n", commits)}, true
	case gitSub(c, "show") && slices.Contains(c.args, omSkewRemoteSHA):
		return cliReply{stdout: fmt.Sprintf("%d\n", 100+int64(age.Seconds()))}, true
	case gitSub(c, "show"):
		return cliReply{stdout: "100\n"}, true
	}
	return cliReply{}, false
}

// TestOMSkewThresholdsReading: a reading of an om whose changed files can change
// the binary says it is below the thresholds only when both of the image-pins
// planner's measures are under; a lag that cannot be read, or one the check has
// no reason to read, leaves it false (gt-pd6jae).
func TestOMSkewThresholdsReading(t *testing.T) {
	t.Parallel()
	code := []string{"internal/review/review.go"}
	for _, tc := range []struct {
		name    string
		commits int
		age     time.Duration
		changed []string
		fails   string // the git subcommand that exits 128
		below   bool
		lagRead bool
		logs    string // a line two readings log exactly once
	}{
		{name: "three commits and a few hours", commits: 3, age: 5 * time.Hour, changed: code, below: true, lagRead: true},
		{name: "one under both thresholds", commits: imagePinsFileBehind - 1, age: imagePinsFileAge - time.Second, changed: code, below: true, lagRead: true},
		{name: "the commit threshold", commits: imagePinsFileBehind, age: 5 * time.Hour, changed: code, lagRead: true},
		{name: "the age threshold", commits: 3, age: imagePinsFileAge, changed: code, lagRead: true},
		{name: "the commit count cannot be read", changed: code, fails: "rev-list", lagRead: true, logs: "how far behind"},
		{name: "a commit time cannot be read", commits: 3, changed: code, fails: "show", lagRead: true, logs: "how far behind"},
		{name: "the changed files cannot be listed", commits: 3, changed: code, fails: "diff"},
		{name: "no changed file reaches the binary", commits: 3, changed: []string{"README.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := omSkewDaemon(t)
			var logs lockedBuffer
			d.logger = log.New(&logs, "", 0)
			mkdirs(t, filepath.Join(d.config.TownRoot, omSkewSrcDir, ".git"))
			cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
				if tc.fails != "" && gitSub(c, tc.fails) {
					return cliReply{stderr: "fatal: bad object", code: 128}
				}
				if gitSub(c, "diff") {
					return cliReply{stdout: strings.Join(tc.changed, "\x00") + "\x00"}
				}
				reply, _ := omSkewLagReply(c, tc.commits, tc.age)
				return reply
			})

			// Twice, as two hourly checks: the log rows count lines across both.
			d.omSkewRead(d.config.TownRoot)
			r := d.omSkewRead(d.config.TownRoot)

			if r.err != nil || r.belowThresholds != tc.below {
				t.Errorf("reading = %+v, want belowThresholds=%t and no error", r, tc.below)
			}
			read := slices.ContainsFunc(cli.argvs("-C"), func(a []string) bool { return slices.Contains(a, "rev-list") })
			if read != tc.lagRead {
				t.Errorf("lag read = %t, want %t", read, tc.lagRead)
			}
			if tc.logs != "" && strings.Count(logs.String(), tc.logs) != 1 {
				t.Errorf("log = %q, want one line naming %q across two readings", logs.String(), tc.logs)
			}
		})
	}
}

// omSkewDiffCalls returns the `git -C <clone> diff ...` calls the check made,
// leaving out the git grep that reads the embed directives.
func omSkewDiffCalls(cli *fakeCLI) [][]string {
	var out [][]string
	for _, args := range cli.argvs("-C") {
		if slices.Contains(args, "diff") {
			out = append(out, args)
		}
	}
	return out
}

// TestOMSkewIgnoresCommitsThatCannotChangeTheBinary: a differing sha whose
// files are all non-binary raises nothing, and one that touches anything that
// can build into om, or that the clone cannot list, raises as before (gt-qozft).
func TestOMSkewIgnoresCommitsThatCannotChangeTheBinary(t *testing.T) {
	t.Parallel()
	failed := cliReply{stderr: "fatal: bad object " + omSkewInstalledSHA, code: 128}
	tests := []struct {
		name    string
		changed []string
		diffErr *cliReply
		noClone bool
		alert   bool
	}{
		{name: "only .om.json", changed: []string{".om.json"}},
		{name: "docs and .om.json", changed: []string{".om.json", "docs/gate.md", "README.md"}},
		{name: "no files differ", changed: nil},
		{name: "a Go source file", changed: []string{"internal/review/review.go"}, alert: true},
		{name: "a Go source file beside .om.json", changed: []string{".om.json", "cmd/om/main.go"}, alert: true},
		{name: "go.mod", changed: []string{"go.mod"}, alert: true},
		{name: "go.sum", changed: []string{"go.sum"}, alert: true},
		{name: "the installer, which no list names", changed: []string{"internal/ominstaller/install.go"}, alert: true},
		{name: "the Makefile", changed: []string{"Makefile"}, alert: true},
		{name: "a path no list names", changed: []string{"assets/prompt.txt"}, alert: true},
		{name: "the clone lacks a sha", diffErr: &failed, alert: true},
		{name: "the clone is missing", noClone: true, alert: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec := omSkewDaemon(t)
			var cli *fakeCLI
			if !tc.noClone {
				cli = omSkewClone(t, d, tc.changed, tc.diffErr)
			}

			d.omSkewOnce()

			if got := len(rec.Escalations()); (got == 1) != tc.alert || got > 1 {
				t.Errorf("escalations = %+v, want alert=%v", rec.Escalations(), tc.alert)
			}
			if cli == nil {
				return
			}
			diffs := omSkewDiffCalls(cli)
			if len(diffs) != 1 {
				t.Fatalf("git calls = %q, want exactly one diff in the clone", diffs)
			}
			wantSrc := filepath.Join(d.config.TownRoot, omSkewSrcDir)
			if diffs[0][1] != wantSrc || !slices.Contains(diffs[0], omSkewInstalledSHA) ||
				!slices.Contains(diffs[0], omSkewRemoteSHA) || diffs[0][len(diffs[0])-1] != "--" {
				t.Errorf("git %q does not diff the two commit ids in %s behind a --", diffs[0], wantSrc)
			}
		})
	}
}

// TestOMSkewClearsWhenTheSkewTurnsOutHarmless: an alert raised for a Go change
// closes once the commits between the two shas are docs only.
func TestOMSkewClearsWhenTheSkewTurnsOutHarmless(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)
	changed := []string{"internal/review/review.go"}
	mkdirs(t, filepath.Join(d.config.TownRoot, omSkewSrcDir, ".git"))
	// omSkewOnce runs on this goroutine, so changed needs no lock.
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "grep") {
			return cliReply{} // a source carrying no directive
		}
		if reply, ok := omSkewLagReply(c, imagePinsFileBehind, 0); ok {
			return reply
		}
		return cliReply{stdout: strings.Join(changed, "\x00") + "\x00"}
	})

	d.omSkewOnce()
	if esc := rec.Escalations(); len(esc) != 1 {
		t.Fatalf("escalations = %+v, want the one raise for a Go change", esc)
	}
	changed = []string{".om.json"}
	d.omSkewOnce()

	if clears := rec.Clears(); len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, omSkewAlertKey) {
		t.Errorf("clears = %+v, want one under %s once only .om.json differs", clears, omSkewAlertKey)
	}
}

// omSkewLaggingDaemon is an omSkewDaemon whose clone diffs a Go file and puts
// the installed om *commits commits behind. A test moves the count between
// checks it calls itself, so the fake reads it on that goroutine: no lock.
func omSkewLaggingDaemon(t *testing.T, commits *int) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	d, rec := omSkewDaemon(t)
	mkdirs(t, filepath.Join(d.config.TownRoot, omSkewSrcDir, ".git"))
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "diff") {
			return cliReply{stdout: "internal/review/review.go\x00"}
		}
		reply, _ := omSkewLagReply(c, *commits, 0)
		return reply
	})
	return d, rec
}

// TestOMSkewBelowThresholdsRaisesNothing: an om under the planner's thresholds
// raises no alert, and an open one is cleared with a message saying why
// (gt-hjw7jv).
func TestOMSkewBelowThresholdsRaisesNothing(t *testing.T) {
	t.Parallel()

	t.Run("no alert is raised", func(t *testing.T) {
		t.Parallel()
		commits := 3
		d, rec := omSkewLaggingDaemon(t, &commits)

		d.omSkewOnce()
		d.omSkewOnce()

		if esc := rec.Escalations(); len(esc) != 0 {
			t.Errorf("escalations = %+v, want none under the thresholds", esc)
		}
		// The one clear is for an alert a previous process may have left open.
		if clears := rec.Clears(); len(clears) != 1 {
			t.Errorf("clears = %+v, want one across two readings", clears)
		}
	})

	t.Run("an open alert is cleared saying why", func(t *testing.T) {
		t.Parallel()
		commits := imagePinsFileBehind
		d, rec := omSkewLaggingDaemon(t, &commits)
		d.omSkewOnce()
		commits = 3
		d.omSkewOnce()

		if esc := rec.Escalations(); len(esc) != 1 {
			t.Fatalf("escalations = %+v, want the one raised at the threshold", esc)
		}
		clears := rec.Clears()
		if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, omSkewAlertKey) {
			t.Fatalf("clears = %+v, want one under %s", clears, omSkewAlertKey)
		}
		if got := clears[0].Reason; !strings.Contains(got, "within the image-pins planner's thresholds") {
			t.Errorf("clear reason = %q, want it to say the installed om is within the planner's thresholds", got)
		}
	})
}

// TestOMSkewBelowThresholdsClearFails: a clear that does not reach the town
// leaves the alert raised, and the next check under the thresholds retries it
// (gt-hjw7jv).
func TestOMSkewBelowThresholdsClearFails(t *testing.T) {
	t.Parallel()
	commits := imagePinsFileBehind
	d, rec := omSkewLaggingDaemon(t, &commits)
	d.omSkewOnce()

	commits = 3
	rec.Fail(notifyfake.KindClear, errors.New("gt escalate: Dolt is unreachable"))
	d.omSkewOnce()
	commits = imagePinsFileBehind
	d.omSkewOnce() // still raised, so nothing new is escalated
	if esc := rec.Escalations(); len(esc) != 1 {
		t.Errorf("escalations = %+v, want the alert still raised after a dropped clear", esc)
	}

	commits = 3
	rec.Fail(notifyfake.KindClear, nil)
	d.omSkewOnce()
	d.omSkewOnce()
	if clears := rec.Clears(); len(clears) != 2 || clears[1].Err != nil {
		t.Errorf("clears = %+v, want the dropped attempt and one retry that reached the town", clears)
	}
}

// TestOMSkewNonBinaryPath pins the match rule: a directory entry is a prefix,
// every other entry the whole path.
func TestOMSkewNonBinaryPath(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		".om.json":           true,
		"docs/gate.md":       true,
		"docs/a/b.md":        true,
		"README.md":          true,
		"docs":               false,
		"docsx/gate.md":      false,
		"sub/.om.json":       false,
		".om.json.bak":       false,
		"main.go":            false,
		"go.mod":             false,
		"scripts/build.sh":   false,
		"internal/docs/x.go": false,
	}
	for path, want := range tests {
		if got := omSkewNonBinaryPath(path); got != want {
			t.Errorf("omSkewNonBinaryPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestOMSkewCloneFailureLogsOnce: a clone that cannot list the range leaves the
// alert raised — the check fails toward alarming — and writes one line per
// distinct failure rather than the same line an hour (gt-8n9hj).
func TestOMSkewCloneFailureLogsOnce(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)
	var logs lockedBuffer
	d.logger = log.New(&logs, "", 0)
	mkdirs(t, filepath.Join(d.config.TownRoot, omSkewSrcDir, ".git"))
	failing := true
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if !gitSub(c, "diff") {
			return cliReply{}
		}
		if failing {
			return cliReply{stderr: "fatal: bad object " + omSkewInstalledSHA, code: 128}
		}
		return cliReply{stdout: "docs/gate.md\x00"}
	})

	d.omSkewOnce()
	d.omSkewOnce()
	d.omSkewOnce()

	if got := strings.Count(logs.String(), "cannot list the files between"); got != 1 {
		t.Errorf("logged the unanswerable diff %d times, want once: %s", got, logs.String())
	}
	if esc := rec.Escalations(); len(esc) != 1 {
		t.Errorf("escalations = %+v, want the one raise for a skew no diff could dismiss", esc)
	}

	// The clone answers, then fails again: the failure is news a second time.
	failing = false
	d.omSkewOnce()
	failing = true
	d.omSkewOnce()

	if got := strings.Count(logs.String(), "cannot list the files between"); got != 2 {
		t.Errorf("logged the diff failure %d times, want it logged again after the clone answered: %s", got, logs.String())
	}
}

// TestOMSkewUnreadableEmbedsLogOnce: the embed directives are the last question
// a reading asks, so a clone that answers the diff but not those logs the
// failure once rather than the same line an hour (gt-8n9hj).
func TestOMSkewUnreadableEmbedsLogOnce(t *testing.T) {
	t.Parallel()
	d, rec := omSkewDaemon(t)
	var logs lockedBuffer
	d.logger = log.New(&logs, "", 0)
	mkdirs(t, filepath.Join(d.config.TownRoot, omSkewSrcDir, ".git"))
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "diff"):
			return cliReply{stdout: "docs/gate.md\x00"}
		case gitSub(c, "grep"):
			return cliReply{stderr: "fatal: bad object " + omSkewRemoteSHA, code: 128}
		}
		return cliReply{}
	})

	d.omSkewOnce()
	d.omSkewOnce()
	d.omSkewOnce()

	if got := strings.Count(logs.String(), "cannot read the om source's embed directives"); got != 1 {
		t.Errorf("logged the unreadable directives %d times, want once: %s", got, logs.String())
	}
	if esc := rec.Escalations(); len(esc) != 1 {
		t.Errorf("escalations = %+v, want the one raise for a skew no probe could dismiss", esc)
	}
}

// TestOMSkewAlertsWhenAnEmbedCarriesAnExemptPath: the allowlist is a shape rule
// and a //go:embed directive is what makes it wrong, so a docs-only change a
// directive reaches keeps the alert (gt-8n9hj). grep is the line git grep
// prints, without the revision prefix the fake adds (omSkewGrepOut).
func TestOMSkewAlertsWhenAnEmbedCarriesAnExemptPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changed string
		grep    string
		grepErr *cliReply
		grepEC  int // exit status of an answer carrying no output
		alert   bool
	}{
		{name: "the file a directive names", changed: "docs/gate.md", grep: "docs/doc.go:3://go:embed gate.md\n", alert: true},
		{name: "a README named from the tree root", changed: "README.md", grep: "main.go:9://go:embed README.md\n", alert: true},
		{name: "a directory a directive names", changed: "docs/templates/a.md", grep: "docs/doc.go:3://go:embed templates\n", alert: true},
		{name: "a glob over the file's directory", changed: "docs/sub/a.md", grep: "docs/doc.go:3://go:embed sub/*.md\n", alert: true},
		{name: "an all: pattern", changed: "docs/.hidden", grep: "docs/doc.go:3://go:embed all:.hidden\n", alert: true},
		{name: "a quoted pattern", changed: "docs/a b.md", grep: "docs/doc.go:3://go:embed \"a b.md\"\n", alert: true},
		{name: "a config file a root directive names", changed: ".om.json", grep: "main.go:7://go:embed .om.json\n", alert: true},
		{name: "a directive in a test source", changed: "docs/gate.md", grep: "docs/doc_test.go:3://go:embed gate.md\n"},
		{name: "a directive outside the file's tree", changed: "docs/gate.md", grep: "internal/x/x.go:3://go:embed docs/gate.md\n"},
		{name: "a directive naming another file", changed: "docs/gate.md", grep: "docs/doc.go:3://go:embed other.md\n"},
		{name: "a line that only starts like a directive", changed: "docs/gate.md", grep: "docs/doc.go:3://go:embedding gate.md\n"},
		{name: "a tree with no directive", changed: "docs/gate.md", grepEC: 1},
		{name: "directives that cannot be read", changed: "docs/gate.md", grepErr: &cliReply{stderr: "fatal: bad object", code: 128}, alert: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec := omSkewDaemon(t)
			mkdirs(t, filepath.Join(d.config.TownRoot, omSkewSrcDir, ".git"))
			withRebuildGTCli(t, d, func(c cliCall) cliReply {
				switch {
				case gitSub(c, "diff"):
					return cliReply{stdout: tc.changed + "\x00"}
				case gitSub(c, "grep"):
					if tc.grepErr != nil {
						return *tc.grepErr
					}
					return cliReply{stdout: omSkewGrepOut(tc.grep), code: tc.grepEC}
				}
				return cliReply{}
			})

			d.omSkewOnce()

			if got := len(rec.Escalations()); (got == 1) != tc.alert || got > 1 {
				t.Errorf("escalations = %+v, want alert=%v", rec.Escalations(), tc.alert)
			}
		})
	}
}

// TestOMSkewEmbedRuleCovers pins what a directive reaches. go:embed patterns
// are relative to the naming package's directory and may not leave it.
func TestOMSkewEmbedRuleCovers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dir     string
		pattern string
		path    string
		covers  bool
	}{
		{name: "an exact name", dir: "", pattern: "README.md", path: "README.md", covers: true},
		{name: "the same name deeper", dir: "", pattern: "README.md", path: "docs/README.md"},
		{name: "a name in another directory", dir: "internal/x", pattern: "gate.md", path: "docs/gate.md"},
		{name: "a sibling directory is unreachable", dir: "internal/x", pattern: "gate.md", path: "internal/y/gate.md"},
		{name: "a directory carries its subtree", dir: "docs", pattern: "templates", path: "docs/templates/a/b.md", covers: true},
		{name: "a directory does not cover its parent", dir: "docs", pattern: "templates", path: "docs/a.md"},
		{name: "a glob over one level", dir: "docs", pattern: "*/*.md", path: "docs/sub/a.md", covers: true},
		{name: "a glob does not cross the directory", dir: "docs", pattern: "*/*.md", path: "docs/sub/deeper/a.md"},
		{name: "a quoted pattern with a space", dir: "docs", pattern: "a b.md", path: "docs/a b.md", covers: true},
		{name: "a pattern for a file the tree lacks", dir: "docs", pattern: "gone.md", path: "docs/gate.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := omSkewEmbedRule{dir: tc.dir, pattern: tc.pattern}
			if got := omSkewEmbedRuleCovers(r, tc.path); got != tc.covers {
				t.Errorf("omSkewEmbedRuleCovers(%+v, %q) = %v, want %v", r, tc.path, got, tc.covers)
			}
		})
	}
}

// TestOMSkewEmbedPatterns pins the reading of one directive: the all: prefix
// comes off, and a quoted pattern holding a space stays one pattern.
func TestOMSkewEmbedPatterns(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		" a b":            {"a", "b"},
		" all:docs/x.md":  {"docs/x.md"},
		" \"a b.md\" c":   {"a b.md", "c"},
		" `a b.md`":       {"a b.md"},
		" all:a all:b":    {"a", "b"},
		"":                nil,
		" \"unterminated": nil,
	}
	for rest, want := range tests {
		if got := omSkewEmbedPatterns(rest); !slices.Equal(got, want) {
			t.Errorf("omSkewEmbedPatterns(%q) = %q, want %q", rest, got, want)
		}
	}
}

// omSkewGrepOut is git grep's answer as it prints it when a revision is
// searched: every line prefixed by that revision as it was written. The
// prefix is part of the shape the parser reads, so the fake carries it rather
// than the bare line (gt-8n9hj).
func omSkewGrepOut(lines string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(lines, "\n"), "\n") {
		if line == "" {
			continue
		}
		b.WriteString(omSkewRemoteSHA + ":" + line + "\n")
	}
	return b.String()
}

// TestParseOMSkewEmbedRules pins the reading of git grep's answer. Searching a
// revision prefixes every line with it, and a parser that reads the sha as the
// file name recognizes no directive at all — the allowlist then dismisses the
// skew a directive says is real, which is the failure this pins (gt-8n9hj).
func TestParseOMSkewEmbedRules(t *testing.T) {
	t.Parallel()
	const rev = omSkewRemoteSHA
	tests := []struct {
		name  string
		out   string
		rules []omSkewEmbedRule
		fails bool
	}{
		{name: "a directive the revision prefixes", out: rev + ":docs/doc.go:3://go:embed gate.md\n",
			rules: []omSkewEmbedRule{{dir: "docs", pattern: "gate.md"}}},
		{name: "a directive at the tree root", out: rev + ":main.go:7://go:embed README.md\n",
			rules: []omSkewEmbedRule{{dir: "", pattern: "README.md"}}},
		{name: "several patterns and a blank line", out: rev + ":docs/doc.go:3://go:embed a.md  b.md\n\n",
			rules: []omSkewEmbedRule{{dir: "docs", pattern: "a.md"}, {dir: "docs", pattern: "b.md"}}},
		{name: "a tree carrying no directive", out: ""},
		{name: "a directive in a test source", out: rev + ":docs/doc_test.go:3://go:embed gate.md\n"},
		{name: "a comment about go:embed", out: rev + ":docs/doc.go:3:\t// a //go:embed directive carries\n"},
		{name: "a line that only starts like a directive", out: rev + ":docs/doc.go:3://go:embedding gate.md\n"},
		{name: "a line carrying no revision", out: "docs/doc.go:3://go:embed gate.md\n", fails: true},
		{name: "a line carrying no line number", out: rev + ":docs/doc.go://go:embed gate.md\n", fails: true},
		{name: "a line that is a file name alone", out: rev + ":docs/doc.go\n", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rules, err := parseOMSkewEmbedRules(rev, tc.out)
			if tc.fails {
				if err == nil {
					t.Errorf("parseOMSkewEmbedRules(%q) = %+v, want the line refused", tc.out, rules)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOMSkewEmbedRules(%q): %v", tc.out, err)
			}
			if !slices.Equal(rules, tc.rules) {
				t.Errorf("parseOMSkewEmbedRules(%q) = %+v, want %+v", tc.out, rules, tc.rules)
			}
		})
	}
}

// TestOMSkewGitNoMatchReadsTheExitStatusItIsGiven: git grep's exit 1 is the
// answer "no line matched", read off the command's own error where it is
// produced, so it does not depend on how another function builds the error it
// hands back (gt-8n9hj).
func TestOMSkewGitNoMatchReadsTheExitStatusItIsGiven(t *testing.T) {
	t.Parallel()
	d, _ := omSkewDaemon(t)
	code := 1
	withRebuildGTCli(t, d, func(cliCall) cliReply { return cliReply{stderr: "no match", code: code} })

	out, err := d.omSkewGitNoMatch("grep", "-n", "x")
	if err != nil || out != "" {
		t.Errorf("exit 1 gave (%q, %v), want it read as a search that matched nothing", out, err)
	}

	code = 128
	if _, err := d.omSkewGitNoMatch("grep", "-n", "x"); err == nil {
		t.Error("exit 128 was read as a search that matched nothing")
	} else if !strings.Contains(err.Error(), "git grep -n x") {
		t.Errorf("exit 128 gave %q, want the call named in the error", err)
	}
	if _, err := d.omSkewGit("grep", "-n", "x"); err == nil {
		t.Error("exit 128 was read as a success")
	}
}

// TestOMSkewRefusesRevisionsThatAreNotCommitIDs: every revision reaches git's
// argv, in the diff and in the grep that reads the embed directives alike, so
// a value that looks like an option is refused before the call rather than
// read as one (gt-8n9hj).
func TestOMSkewRefusesRevisionsThatAreNotCommitIDs(t *testing.T) {
	t.Parallel()
	d, _ := omSkewDaemon(t)
	cli := omSkewClone(t, d, []string{".om.json"}, nil)

	if _, err := d.omSkewChangedPaths(d.config.TownRoot, omSkewInstalledSHA, omSkewRemoteSHA); err != nil {
		t.Fatalf("a diff of two commit ids failed: %v", err)
	}
	if _, err := d.omSkewEmbedRules(d.config.TownRoot, omSkewRemoteSHA); err != nil {
		t.Fatalf("a grep of a commit id failed: %v", err)
	}
	if _, err := d.omSkewChangedPaths(d.config.TownRoot, "--output=/tmp/om-skew", omSkewRemoteSHA); err == nil {
		t.Error("an installed revision that is not a commit id was diffed")
	}
	if _, err := d.omSkewChangedPaths(d.config.TownRoot, omSkewInstalledSHA, "--no-index"); err == nil {
		t.Error("a remote revision that is not a commit id was diffed")
	}
	if _, err := d.omSkewEmbedRules(d.config.TownRoot, "--no-index"); err == nil {
		t.Error("a revision that is not a commit id was searched for embed directives")
	}
	if diffs := omSkewDiffCalls(cli); len(diffs) != 1 {
		t.Errorf("git calls = %q, want only the one diff of two commit ids", diffs)
	}
}

// TestOMSkewRefreshLogsAFailureOnce: a refresh that fails logs once per distinct
// message, and a refresh that succeeds lets the same failure log again.
func TestOMSkewRefreshLogsAFailureOnce(t *testing.T) {
	t.Parallel()
	d, _ := omSkewDaemon(t)
	var logs lockedBuffer
	d.logger = log.New(&logs, "", 0)
	failure := errors.New("the om origin did not answer")
	d.omSkew.refresh = func(string) (string, error) { return "", failure }
	d.omSkewRefresh(d.config.TownRoot)
	d.omSkewRefresh(d.config.TownRoot)
	if n := strings.Count(logs.String(), failure.Error()); n != 1 {
		t.Fatalf("logged the failure %d times across two calls, want once", n)
	}
	d.omSkew.refresh = func(string) (string, error) { return "", nil }
	d.omSkewRefresh(d.config.TownRoot)
	d.omSkew.refresh = func(string) (string, error) { return "", failure }
	d.omSkewRefresh(d.config.TownRoot)
	if n := strings.Count(logs.String(), failure.Error()); n != 2 {
		t.Errorf("logged the failure %d times, want it again after a recovery", n)
	}
}

// TestOMSkewRefreshRunsTheCloneWhenNoFieldIsSet: an unset refresh field runs
// omSkewRefreshClone on the check's own clone.
func TestOMSkewRefreshRunsTheCloneWhenNoFieldIsSet(t *testing.T) {
	t.Parallel()
	d, _ := omSkewDaemon(t)
	d.omSkew.refresh = nil // the shared builder no-ops it; this test wants the clone
	var calls []string
	d.execCmd = hmSkewRunner(t, hmSkewAnswers{calls: &calls})
	d.omSkewRefresh(d.config.TownRoot)
	if len(calls) != 3 || !strings.Contains(calls[1], " clone ") || !strings.Contains(calls[2], " fetch ") {
		t.Errorf("calls = %q, want a clone into the check's own clone and a fetch", calls)
	}
}

// TestOMSkewRefreshesBeforeItReads: differing shas refresh the clone before the diff so a short lag raises nothing; equal shas refresh nothing; a failed refresh still alarms (gt-wm7cwr).
func TestOMSkewRefreshesBeforeItReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, remote string
		fail         bool
	}{
		{name: "a short lag", remote: omSkewRemoteSHA},
		{name: "equal shas", remote: omSkewInstalledSHA},
		{name: "a failed refresh", remote: omSkewRemoteSHA, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, rec := omSkewDaemon(t)
			d.omSkew.refresh = nil // drive the check's own clone through the fake git
			d.omHeadFn = func(string) (string, error) { return tc.remote, nil }
			mkdirs(t, filepath.Join(d.config.TownRoot, omSkewSrcDir, ".git"))
			tried := false
			withRebuildGTCli(t, d, func(c cliCall) cliReply {
				switch {
				case slices.Contains(c.args, "get-url"):
					tried = true
					if tc.fail {
						return cliReply{code: 128}
					}
					return cliReply{stdout: "https://example.invalid/om.git\n"}
				case gitSub(c, "diff"):
					if tc.fail || !tried { // a diff before the refresh reads a clone nothing kept current
						return cliReply{code: 128}
					}
					return cliReply{stdout: "internal/review/review.go\x00"}
				}
				r, _ := omSkewLagReply(c, 2, 3*time.Hour)
				return r
			})
			d.omSkewOnce()
			if got := len(rec.Escalations()) == 1; got != tc.fail || tried != (tc.remote != omSkewInstalledSHA) {
				t.Errorf("alert %t, refresh attempted %t; want %t and %t", got, tried, tc.fail, tc.remote != omSkewInstalledSHA)
			}
		})
	}
}
