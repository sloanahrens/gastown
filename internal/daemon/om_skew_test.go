package daemon

import (
	"context"
	"errors"
	"log"
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

// omSkewClone gives the daemon the rebuild_om job's clone and answers its git
// diff with changed (NUL-separated, as `git diff -z` prints it) or, when
// diffErr is set, with that failure. It returns the fake so a test can read the
// calls the check made.
func omSkewClone(t *testing.T, d *Daemon, changed []string, diffErr *cliReply) *fakeCLI {
	t.Helper()
	mkdirs(t, filepath.Join(d.config.TownRoot, rebuildOMSrcDir, ".git"))
	return withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if c.name == "git" && slices.Contains(c.args, "diff") {
			if diffErr != nil {
				return *diffErr
			}
			return cliReply{stdout: strings.Join(changed, "\x00") + "\x00"}
		}
		return cliReply{}
	})
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
			wantSrc := filepath.Join(d.config.TownRoot, rebuildOMSrcDir)
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
	mkdirs(t, filepath.Join(d.config.TownRoot, rebuildOMSrcDir, ".git"))
	// omSkewOnce runs on this goroutine, so changed needs no lock.
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "grep") {
			return cliReply{} // a source carrying no directive
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
	mkdirs(t, filepath.Join(d.config.TownRoot, rebuildOMSrcDir, ".git"))
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
	mkdirs(t, filepath.Join(d.config.TownRoot, rebuildOMSrcDir, ".git"))
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
			mkdirs(t, filepath.Join(d.config.TownRoot, rebuildOMSrcDir, ".git"))
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
