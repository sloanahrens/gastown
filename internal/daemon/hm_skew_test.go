package daemon

import (
	"bytes"
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
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

const (
	hmSkewTestPath      = "/fake/bin/hm"
	hmSkewTestInstalled = "1111111111111111111111111111111111111111"
	hmSkewTestRemote    = "2222222222222222222222222222222222222222"
	hmSkewTestURL       = "https://example.invalid/hm.git"
)

// hmSkewAnswers is what the fake hm and git answer. The zero value answers
// nothing useful; hmSkewBehindAnswers is the town the tests start from.
type hmSkewAnswers struct {
	version      string   // stdout of `hm version`
	versionErr   bool     // `hm version` fails to run
	originErr    bool     // the rig's origin url cannot be read
	noOrigin     bool     // the rig's origin url is empty
	lsRemoteErr  bool     // the ls-remote call fails
	lsRemote     string   // stdout of ls-remote
	diffErr      bool     // the clone's diff fails
	changed      []string // the paths the clone's diff names
	lacksInstall bool     // the clone has no commit for the installed sha
	probeErr     bool     // the clone cannot answer whether it has the installed sha
	noClone      bool     // the job-owned clone does not exist yet, so the monitor clones it
	cloneErr     bool     // the clone fails, leaving a partial directory behind
	fetchErr     bool     // the fetch of origin's main into the clone fails
}

// hmSkewBehindAnswers is an hm at installed sha A, origin's main at B, and a diff
// naming one code path: a skew that must alarm.
func hmSkewBehindAnswers() hmSkewAnswers {
	return hmSkewAnswers{
		version:  "hm " + hmSkewTestInstalled + "\n",
		lsRemote: hmSkewTestRemote + "\trefs/heads/main\n",
		changed:  []string{"internal/cli/cmd_query.go"},
	}
}

// hmSkewInStepAnswers is an hm at origin's main: nothing to alarm about.
func hmSkewInStepAnswers() hmSkewAnswers {
	a := hmSkewBehindAnswers()
	a.version = "hm " + hmSkewTestRemote + "\n"
	a.changed = nil
	return a
}

// hmSkewExit is an error carrying an exit status, as a git call that exits
// non-zero returns.
type hmSkewExit int

func (e hmSkewExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e hmSkewExit) ExitCode() int { return int(e) }

// hmSkewRunner answers the daemon's hm and git calls from a. An unexpected call
// fails the test, so a check that reads more than the design says is caught.
func hmSkewRunner(t *testing.T, a hmSkewAnswers) cmdRunFunc {
	t.Helper()
	return func(cmd *exec.Cmd) ([]byte, []byte, error) {
		joined := strings.Join(cmd.Args, " ")
		var out string
		var err error
		switch {
		case cmd.Args[0] == hmSkewTestPath:
			out = a.version
			if a.versionErr {
				err = errors.New("exec: hm: not found")
			}
		case strings.Contains(joined, " remote get-url origin"):
			out = hmSkewTestURL
			if a.noOrigin {
				out = ""
			}
			if a.originErr {
				err = errors.New("fatal: not a git repository")
			}
		case strings.Contains(joined, " ls-remote "):
			out = a.lsRemote
			if a.lsRemoteErr {
				err = errors.New("fatal: unable to access remote")
			}
		case strings.Contains(joined, " clone "):
			// The last argument is the clone's directory. A clone that succeeds
			// leaves a repository there; one that fails leaves a partial one.
			dst := cmd.Args[len(cmd.Args)-1]
			if a.cloneErr {
				mkdirs(t, filepath.Join(dst, ".git"))
				err = errors.New("fatal: unable to access remote")
				break
			}
			mkdirs(t, filepath.Join(dst, ".git"))
		case strings.Contains(joined, " fetch "):
			// The clone is the call's -C directory: git fetches into a repository
			// only, so a fetch before the clone exists fails as it would for real.
			_, statErr := os.Stat(filepath.Join(cmd.Args[2], ".git"))
			switch {
			case a.fetchErr:
				err = errors.New("fatal: unable to access remote")
			case statErr != nil:
				err = errors.New("fatal: not a git repository")
			}
		case strings.Contains(joined, " diff --name-only "):
			if a.diffErr {
				err = errors.New("fatal: bad revision")
			}
			for _, p := range a.changed {
				out += p + "\x00"
			}
		case strings.Contains(joined, " rev-parse --verify -q "):
			switch {
			case a.probeErr:
				err = hmSkewExit(128)
			case a.lacksInstall:
				err = hmSkewExit(1)
			default:
				out = hmSkewTestInstalled
			}
		default:
			t.Errorf("unexpected call: %s", joined)
			err = errors.New("unexpected call")
		}
		if _, werr := io.WriteString(cmd.Stdout, out); werr != nil {
			t.Errorf("writing the fake output of %s: %v", joined, werr)
		}
		return nil, nil, err
	}
}

// hmSkewDaemon is a Daemon whose hm and git answers come from a, with its log
// and everything it escalates or clears recorded. Nothing starts a process or
// reaches a remote, and the clone lives under the test's own town.
func hmSkewDaemon(t *testing.T, a hmSkewAnswers) (*Daemon, *notifyfake.Recorder, *bytes.Buffer) {
	t.Helper()
	rec := notifyfake.New()
	var logs bytes.Buffer
	town := t.TempDir()
	if !a.noClone {
		mkdirs(t, filepath.Join(town, hmSkewCloneDir, ".git"))
	}
	d := &Daemon{
		logger:   log.New(&logs, "", 0),
		config:   &Config{TownRoot: town},
		notifier: rec,
		clock:    newFixedClock(),
		ctx:      t.Context(),
		execCmd:  hmSkewRunner(t, a),
	}
	d.hmSkew.pathFn = func() string { return hmSkewTestPath }
	return d, rec, &logs
}

// TestHMSkewEscalatesWhenBehindOnACodePath: an installed hm behind origin's main
// on a code path escalates once under hm-skew, and the alert names both shas
// and the command that clears it (acceptance 1).
func TestHMSkewEscalatesWhenBehindOnACodePath(t *testing.T) {
	t.Parallel()
	d, rec, _ := hmSkewDaemon(t, hmSkewBehindAnswers())

	d.hmSkewOnce()

	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want exactly one for a skew on a code path", esc)
	}
	got := esc[0].Escalation
	if got.Fingerprint != hmSkewAlertKey {
		t.Errorf("fingerprint = %q, want %q", got.Fingerprint, hmSkewAlertKey)
	}
	if got.Severity != "MEDIUM" {
		t.Errorf("severity = %q, want MEDIUM as the om-skew alert", got.Severity)
	}
	for _, want := range []string{hmSkewTestInstalled[:8], hmSkewTestRemote[:8], "make install", hmSkewSource} {
		if !strings.Contains(got.Reason+" "+got.Description, want) {
			t.Errorf("alert %q / %q does not name %q", got.Description, got.Reason, want)
		}
	}
}

// TestHMSkewIgnoresDocsOnlyAndEqual: a diff over markdown and docs alone is not a
// skew, and an hm at origin's tip, in full or as the short sha hm prints, raises
// nothing (acceptance 2).
func TestHMSkewIgnoresDocsOnlyAndEqual(t *testing.T) {
	t.Parallel()

	t.Run("a diff naming only docs and markdown", func(t *testing.T) {
		t.Parallel()
		a := hmSkewBehindAnswers()
		a.changed = []string{"docs/x.md", "README.md"}
		d, rec, _ := hmSkewDaemon(t, a)

		d.hmSkewOnce()

		if esc := rec.Escalations(); len(esc) != 0 {
			t.Errorf("escalations = %+v, want none for a diff over docs and markdown", esc)
		}
	})

	t.Run("an installed hm equal to origin main", func(t *testing.T) {
		t.Parallel()
		// The clone would name a code path if it were asked, so the alarm only
		// stays quiet because the equal sha is read as no skew before any diff.
		a := hmSkewInStepAnswers()
		a.changed = []string{"internal/cli/cmd_query.go"}
		d, rec, _ := hmSkewDaemon(t, a)

		d.hmSkewOnce()

		if esc := rec.Escalations(); len(esc) != 0 {
			t.Errorf("escalations = %+v, want none for an hm at origin/main", esc)
		}
	})

	t.Run("an installed short sha that prefixes origin main", func(t *testing.T) {
		t.Parallel()
		a := hmSkewInStepAnswers()
		a.version = "hm " + hmSkewTestRemote[:7] + "\n"
		d, rec, _ := hmSkewDaemon(t, a)

		d.hmSkewOnce()

		if esc := rec.Escalations(); len(esc) != 0 {
			t.Errorf("escalations = %+v, want none for a short sha of origin/main", esc)
		}
	})
}

// TestHMSkewUnreadableIsALogLine: a reading that cannot be taken logs a line and
// escalates nothing, whether the version carries no sha, the remote cannot be
// read, or the clone cannot answer (acceptance 3).
func TestHMSkewUnreadableIsALogLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a    func() hmSkewAnswers
	}{
		{"a version with no sha", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.version = "hm: unknown\n"
			return a
		}},
		{"an hm that fails to run", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.versionErr = true
			return a
		}},
		{"a rig whose origin cannot be read", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.originErr = true
			return a
		}},
		{"a rig with no origin url", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.noOrigin = true
			return a
		}},
		{"a failed ls-remote", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.lsRemoteErr = true
			return a
		}},
		{"an ls-remote with no main tip", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.lsRemote = ""
			return a
		}},
		{"a clone that cannot answer whether it has the installed commit", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.probeErr = true
			return a
		}},
		{"a clone that cannot be made on first use", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.noClone = true
			a.cloneErr = true
			return a
		}},
		{"a fetch of origin's main that fails", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.fetchErr = true
			return a
		}},
		{"a clone whose diff fails and that has the installed commit", func() hmSkewAnswers {
			a := hmSkewBehindAnswers()
			a.diffErr = true
			return a
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec, logs := hmSkewDaemon(t, tc.a())

			d.hmSkewOnce()

			if !strings.Contains(logs.String(), "hm_skew:") {
				t.Errorf("log = %q, want a line naming the unreadable reading", logs.String())
			}
			if esc := rec.Escalations(); len(esc) != 0 {
				t.Errorf("escalations = %+v, want none for a reading that could not be taken", esc)
			}
			if clears := rec.Clears(); len(clears) != 0 {
				t.Errorf("clears = %+v, want none for a reading that could not be taken", clears)
			}
		})
	}
}

// TestHMSkewClonesTheCloneWhenItIsAbsent: with no clone yet, the monitor clones
// the hm origin itself and judges the skew from it, so a fresh town alarms with
// no producer in front of the check.
func TestHMSkewClonesTheCloneWhenItIsAbsent(t *testing.T) {
	t.Parallel()
	a := hmSkewBehindAnswers()
	a.noClone = true
	d, rec, _ := hmSkewDaemon(t, a)

	d.hmSkewOnce()

	if esc := rec.Escalations(); len(esc) != 1 {
		t.Fatalf("escalations = %+v, want one from a skew judged on the clone the monitor made", esc)
	}
}

// TestHMSkewFailedCloneLeavesNoPartialDirectory: a clone that fails leaves no
// directory behind, so the next reading clones again rather than failing on a
// partial clone for good.
func TestHMSkewFailedCloneLeavesNoPartialDirectory(t *testing.T) {
	t.Parallel()
	a := hmSkewBehindAnswers()
	a.noClone = true
	a.cloneErr = true
	d, _, _ := hmSkewDaemon(t, a)

	d.hmSkewOnce()

	clone := filepath.Join(d.config.TownRoot, hmSkewCloneDir)
	if _, err := os.Stat(clone); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want the partial clone removed", clone, err)
	}
}

// TestHMSkewRepeatsUnderOneKey: two consecutive skewed readings each escalate,
// both under the one key, so gt escalate records a persisting skew as a repeat
// on one bead rather than a new key an hour (acceptance 4).
func TestHMSkewRepeatsUnderOneKey(t *testing.T) {
	t.Parallel()
	d, rec, _ := hmSkewDaemon(t, hmSkewBehindAnswers())

	d.hmSkewOnce()
	d.hmSkewOnce()

	esc := rec.Escalations()
	if len(esc) != 2 {
		t.Fatalf("escalations = %+v, want one per skewed reading", esc)
	}
	for i, e := range esc {
		if e.Escalation.Fingerprint != hmSkewAlertKey {
			t.Errorf("escalation %d fingerprint = %q, want %q", i, e.Escalation.Fingerprint, hmSkewAlertKey)
		}
	}
	if clears := rec.Clears(); len(clears) != 0 {
		t.Errorf("clears = %+v, want none while the skew persists", clears)
	}
}

// TestHMSkewAlarmsWhenTheCloneLacksTheInstalledCommit: an installed commit the
// clone does not have is not main's history, so it is a skew even though the
// diff cannot be listed.
func TestHMSkewAlarmsWhenTheCloneLacksTheInstalledCommit(t *testing.T) {
	t.Parallel()
	a := hmSkewBehindAnswers()
	a.diffErr = true
	a.lacksInstall = true
	d, rec, _ := hmSkewDaemon(t, a)

	d.hmSkewOnce()

	if esc := rec.Escalations(); len(esc) != 1 {
		t.Errorf("escalations = %+v, want one for an installed commit the clone lacks", esc)
	}
}

// TestHMSkewClearsWhenTheSkewEnds: once the installed hm reaches origin's main,
// the alert closes, and only once for a process that had nothing open.
func TestHMSkewClearsWhenTheSkewEnds(t *testing.T) {
	t.Parallel()
	d, rec, _ := hmSkewDaemon(t, hmSkewBehindAnswers())
	d.hmSkewOnce()

	d.execCmd = hmSkewRunner(t, hmSkewInStepAnswers())
	d.hmSkewOnce()
	d.hmSkewOnce()

	clears := rec.Clears()
	if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, hmSkewAlertKey) {
		t.Errorf("clears = %+v, want exactly one under %s once the skew ends", clears, hmSkewAlertKey)
	}
}

// TestHMSkewMonitorChecksAtStartAndThenHourly: the monitor reads at start and
// then once an hour, and not in between.
func TestHMSkewMonitorChecksAtStartAndThenHourly(t *testing.T) {
	t.Parallel()
	d, _, _ := hmSkewDaemon(t, hmSkewInStepAnswers())
	clk := newFixedClock()
	d.clock = clk
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.ctx = ctx

	// One signal per check, sent as the check reads the installed hm: the clock
	// moves only after a reading, so one tick buys exactly one check.
	reads := make(chan struct{}, 8)
	inner := hmSkewRunner(t, hmSkewInStepAnswers())
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		if cmd.Args[0] == hmSkewTestPath {
			reads <- struct{}{}
		}
		return inner(cmd)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.runHMSkewMonitor()
	}()
	nextCheck := func(what string) {
		t.Helper()
		waitForSignal(t, reads, "the monitor took no reading "+what)
	}

	nextCheck("at start")
	clk.BlockUntil(1) // the ticker is armed before the clock moves
	clk.Advance(hmSkewInterval)
	nextCheck("an hour in")
	cancel()
	<-done

	select {
	case <-reads:
		t.Error("the monitor checked without an hour having passed")
	default:
	}
}

// TestHMSkewRefusesRevisionsThatAreNotCommitIDs: a revision that is not a commit
// id is refused before it reaches git's argv, so a value shaped like an option
// is never read as one. The runner answers nothing, so a call that got past the
// guard would also fail the test as an unexpected call.
func TestHMSkewRefusesRevisionsThatAreNotCommitIDs(t *testing.T) {
	t.Parallel()
	d, _, _ := hmSkewDaemon(t, hmSkewBehindAnswers())

	for _, bad := range []string{"--output=/x", "1111111;rm"} {
		if _, err := d.hmSkewBehind(d.config.TownRoot, hmSkewTestURL, bad, hmSkewTestRemote); err == nil {
			t.Errorf("hmSkewBehind(%q, ...) = nil error, want a refusal before git runs", bad)
		}
	}
}

// TestHMSkewDroppedEscalationIsLoggedAndRetried: an escalation that never reached
// the town is logged, and the next reading escalates again, so a skew the town
// never received is not treated as delivered.
func TestHMSkewDroppedEscalationIsLoggedAndRetried(t *testing.T) {
	t.Parallel()
	d, rec, logs := hmSkewDaemon(t, hmSkewBehindAnswers())
	clk := newFixedClock()
	d.clock = clk
	rec.Fail(notifyfake.KindEscalate, errors.New("gt escalate: Dolt is unreachable"))

	// Each failed attempt backs off on the daemon's clock, so the drop is driven
	// rather than waited out.
	runOnClock(t, clk, time.Second, d.hmSkewOnce)

	if !strings.Contains(logs.String(), "did not reach the town") {
		t.Errorf("log = %q, want the dropped escalation named", logs.String())
	}

	rec.Fail(notifyfake.KindEscalate, nil)
	d.hmSkewOnce()

	esc := rec.Escalations()
	if len(esc) != maxEscalationRetries+1 {
		t.Fatalf("escalations = %d, want the %d failed attempts and the retry on the next reading", len(esc), maxEscalationRetries)
	}
	if last := esc[len(esc)-1]; last.Err != nil {
		t.Errorf("the retry on the next reading failed too: %v", last.Err)
	}
}

// TestHMSkewUnclearedAlertRetriesOnTheNextCleanReading: a clear that never
// reached the town leaves the alert open in the process, so the next clean
// reading clears it again rather than forgetting it.
func TestHMSkewUnclearedAlertRetriesOnTheNextCleanReading(t *testing.T) {
	t.Parallel()
	d, rec, logs := hmSkewDaemon(t, hmSkewBehindAnswers())
	d.hmSkewOnce()

	d.execCmd = hmSkewRunner(t, hmSkewInStepAnswers())
	rec.Fail(notifyfake.KindClear, errors.New("gt escalate: Dolt is unreachable"))
	d.hmSkewOnce()

	if !strings.Contains(logs.String(), "did not reach the town") {
		t.Errorf("log = %q, want the undelivered clear named", logs.String())
	}

	rec.Fail(notifyfake.KindClear, nil)
	d.hmSkewOnce()

	clears := rec.Clears()
	if len(clears) != 2 {
		t.Fatalf("clears = %+v, want the failed clear and the retry on the next clean reading", clears)
	}
	if last := clears[len(clears)-1]; last.Err != nil || !slices.Contains(last.Fingerprints, hmSkewAlertKey) {
		t.Errorf("the retry = %+v, want a delivered clear under %s", last, hmSkewAlertKey)
	}
}

// TestHMSkewUnreadableLogsOncePerChange: an unchanged unreadable reading logs one
// line, however many checks repeat it, and a different failure is logged anew.
func TestHMSkewUnreadableLogsOncePerChange(t *testing.T) {
	t.Parallel()
	a := hmSkewBehindAnswers()
	a.lsRemoteErr = true
	d, _, logs := hmSkewDaemon(t, a)

	d.hmSkewOnce()
	d.hmSkewOnce()
	if n := strings.Count(logs.String(), "hm_skew:"); n != 1 {
		t.Errorf("log lines = %d, want one for an unreadable reading that did not change", n)
	}

	b := hmSkewBehindAnswers()
	b.versionErr = true
	d.execCmd = hmSkewRunner(t, b)
	d.hmSkewOnce()
	if n := strings.Count(logs.String(), "hm_skew:"); n != 2 {
		t.Errorf("log lines = %d, want the new failure logged once more", n)
	}
}

// TestHMSkewIsNotReadWithoutAConfig: with no daemon config there is no town to
// read, so a check reads nothing and alerts on nothing.
func TestHMSkewIsNotReadWithoutAConfig(t *testing.T) {
	t.Parallel()
	d, rec, logs := hmSkewDaemon(t, hmSkewBehindAnswers())
	d.config = nil

	d.hmSkewOnce()

	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("escalations = %+v, want none without a config", esc)
	}
	if logs.Len() != 0 {
		t.Errorf("log = %q, want nothing without a config", logs.String())
	}
}

// TestHMSkewMonitorNeedsAContext: a start without a daemon context starts
// nothing, so a later start with one still takes its reading at start.
func TestHMSkewMonitorNeedsAContext(t *testing.T) {
	t.Parallel()
	d, _, _ := hmSkewDaemon(t, hmSkewInStepAnswers())
	reads := make(chan struct{}, 8)
	inner := hmSkewRunner(t, hmSkewInStepAnswers())
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		if cmd.Args[0] == hmSkewTestPath {
			reads <- struct{}{}
		}
		return inner(cmd)
	}

	d.ctx = nil
	d.startHMSkewMonitor() // refused: there is no context to stop the monitor on

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.ctx = ctx
	d.startHMSkewMonitor()
	waitForSignal(t, reads, "the monitor took no reading once it had a context")
}

// TestHMSkewPathOrder: hm is read from the PATH first, then from the go install
// location under home, and by name when neither exists.
func TestHMSkewPathOrder(t *testing.T) {
	t.Parallel()
	onPath := func(string) (string, error) { return "/usr/bin/hm", nil }
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }
	cases := []struct {
		name     string
		lookPath func(string) (string, error)
		home     string
		want     string
	}{
		{"hm on the PATH wins", onPath, "/home/x", "/usr/bin/hm"},
		{"the go install location when PATH has no hm", notOnPath, "/home/x", "/home/x/go/bin/hm"},
		{"hm by name with no home", notOnPath, "", "hm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hmSkewPathOf(tc.lookPath, tc.home); got != tc.want {
				t.Errorf("hmSkewPathOf(...) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseHMVersion: the sha is the first field of hm's "hm <sha>" line, in
// lower case, and output without one is no reading.
func TestParseHMVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, out, want string
		wantErr         bool
	}{
		{"a full sha", "hm 0123456789abcdef0123456789abcdef01234567\n", "0123456789abcdef0123456789abcdef01234567", false},
		{"a short sha with trailing text", "hm 0123abc (dirty)\n", "0123abc", false},
		{"an upper-case sha", "hm 0123ABC\n", "0123abc", false},
		{"a version line with no sha", "hm unknown\n", "", true},
		{"an om line", "om version 0123abc\n", "", true},
		{"no output", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseHMVersion(tc.out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseHMVersion(%q) error = %v, want error %v", tc.out, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("parseHMVersion(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}
