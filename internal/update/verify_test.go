package update

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	verifyTestWant    = "0123456789abcdef0123456789abcdef01234567" // a 40-character main sha
	verifyTestShort   = "012345678"                                // the 9 characters gt version prints
	verifyTestOld     = "fedcba987"                                // a build that is not the target
	verifyTestImage   = "0123456"
	verifyTestGreen   = "GREEN tick 2m ago"
	verifyTestStart   = "2026-11-03T12:00:00Z"
	verifyTestPoll    = 15 * time.Second
	verifyTestTimeout = time.Minute
)

// verifyAnswer is one DaemonVersion result.
type verifyAnswer struct {
	version string
	err     error
}

// fakeVerifyProbes answers each probe with what a test set, recording calls.
// The daemon answers its script in order and repeats the last answer once the
// script runs out.
type fakeVerifyProbes struct {
	gt, image, health                             string
	errGT, errImage, errHealth                    error
	daemon                                        []verifyAnswer
	daemonCalls, gtCalls, imageCalls, healthCalls int
}

func (p *fakeVerifyProbes) GTVersion(context.Context) (string, error) {
	p.gtCalls++
	return p.gt, p.errGT
}

func (p *fakeVerifyProbes) DaemonVersion(context.Context) (string, error) {
	answer := verifyAnswer{}
	if len(p.daemon) > 0 {
		i := min(p.daemonCalls, len(p.daemon)-1)
		answer = p.daemon[i]
	}
	p.daemonCalls++
	return answer.version, answer.err
}

func (p *fakeVerifyProbes) ImageLabel(context.Context) (string, error) {
	p.imageCalls++
	return p.image, p.errImage
}

func (p *fakeVerifyProbes) Health(context.Context) (string, error) {
	p.healthCalls++
	return p.health, p.errHealth
}

// greenProbes is every probe matching want, for tests that vary one of them.
func greenProbes() *fakeVerifyProbes {
	return &fakeVerifyProbes{
		gt:     verifyTestShort,
		image:  verifyTestShort,
		health: verifyTestGreen,
		daemon: []verifyAnswer{{version: verifyTestWant}},
	}
}

// fakeVerifyClock is a clock the fake sleep advances, so no test waits.
type fakeVerifyClock struct{ now time.Time }

func (c *fakeVerifyClock) Now() time.Time { return c.now }

// verifySleeps returns a sleep that advances clock by d and records d, and the
// slice it records into.
func verifySleeps(clock *fakeVerifyClock) (func(context.Context, time.Duration) error, *[]time.Duration) {
	sleeps := &[]time.Duration{}
	sleep := func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*sleeps = append(*sleeps, d)
		clock.now = clock.now.Add(d)
		return nil
	}
	return sleep, sleeps
}

func TestWaitForRestartSeesTheNewDaemon(t *testing.T) {
	t.Parallel()
	probes := &fakeVerifyProbes{daemon: []verifyAnswer{
		{version: verifyTestOld},
		{version: verifyTestOld},
		{err: errors.New("daemon not answering")},
		{version: verifyTestShort},
	}}
	clock := &fakeVerifyClock{now: parseVerifyTime(t)}
	sleep, sleeps := verifySleeps(clock)

	restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)
	if err != nil {
		t.Fatalf("WaitForRestart: %v", err)
	}
	if !restarted {
		t.Fatal("WaitForRestart reported no restart after the daemon answered the target")
	}
	if len(*sleeps) != 3 {
		t.Fatalf("slept %d times, want 3", len(*sleeps))
	}
	if probes.daemonCalls != 4 {
		t.Fatalf("polled the daemon %d times, want 4", probes.daemonCalls)
	}
}

func TestWaitForRestartDoesNotTrustAVersionWithAnError(t *testing.T) {
	t.Parallel()
	probes := &fakeVerifyProbes{daemon: []verifyAnswer{
		{version: verifyTestShort, err: errors.New("daemon closing its socket")},
		{version: verifyTestShort},
	}}
	clock := &fakeVerifyClock{now: parseVerifyTime(t)}
	sleep, sleeps := verifySleeps(clock)

	restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)
	if err != nil {
		t.Fatalf("WaitForRestart: %v", err)
	}
	if !restarted {
		t.Fatal("WaitForRestart missed the daemon answering the target on its second poll")
	}
	if probes.daemonCalls != 2 {
		t.Fatalf("polled the daemon %d times, want 2: an answer carrying an error is not a restart", probes.daemonCalls)
	}
	if len(*sleeps) != 1 {
		t.Fatalf("slept %d times, want 1", len(*sleeps))
	}
}

func TestWaitForRestartPollsOnceWhenTimeoutPassesDuringSleep(t *testing.T) {
	t.Parallel()
	// The daemon answers the target only on the poll that lands exactly when
	// the timeout passes, the one a sleep ends on.
	probes := &fakeVerifyProbes{daemon: []verifyAnswer{
		{version: verifyTestOld},
		{version: verifyTestOld},
		{version: verifyTestOld},
		{version: verifyTestOld},
		{version: verifyTestShort},
	}}
	clock := &fakeVerifyClock{now: parseVerifyTime(t)}
	sleep, _ := verifySleeps(clock)

	restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)
	if err != nil {
		t.Fatalf("WaitForRestart: %v", err)
	}
	if !restarted {
		t.Fatal("WaitForRestart missed the daemon answering the target on the poll at the timeout")
	}
	if probes.daemonCalls != 5 {
		t.Fatalf("polled the daemon %d times, want 5: the poll that ends the timeout must run", probes.daemonCalls)
	}
}

func TestWaitForRestartTimesOutAndStopsOnCancel(t *testing.T) {
	t.Parallel()
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		probes := &fakeVerifyProbes{daemon: []verifyAnswer{{version: verifyTestOld}}}
		start := parseVerifyTime(t)
		clock := &fakeVerifyClock{now: start}
		sleep, sleeps := verifySleeps(clock)

		restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)
		if err != nil {
			t.Fatalf("WaitForRestart: %v", err)
		}
		if restarted {
			t.Fatal("WaitForRestart reported a restart though the daemon never changed")
		}
		if elapsed := clock.now.Sub(start); elapsed < verifyTestTimeout || elapsed > verifyTestTimeout+verifyTestPoll {
			t.Fatalf("wait ended after %s, want the timeout %s plus at most one poll", elapsed, verifyTestTimeout)
		}
		if len(*sleeps) == 0 {
			t.Fatal("wait returned before polling the daemon")
		}
	})

	t.Run("cancel during the wait", func(t *testing.T) {
		t.Parallel()
		probes := &fakeVerifyProbes{daemon: []verifyAnswer{{version: verifyTestOld}}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeVerifyClock{now: parseVerifyTime(t)}
		sleep := func(sleepCtx context.Context, _ time.Duration) error {
			cancel()
			return sleepCtx.Err()
		}

		restarted, err := WaitForRestart(ctx, clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)
		if restarted {
			t.Fatal("WaitForRestart reported a restart after the context was canceled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitForRestart error = %v, want context.Canceled", err)
		}
	})

	t.Run("cancel before the first poll", func(t *testing.T) {
		t.Parallel()
		probes := &fakeVerifyProbes{daemon: []verifyAnswer{{version: verifyTestOld}}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		clock := &fakeVerifyClock{now: parseVerifyTime(t)}
		sleep, sleeps := verifySleeps(clock)

		restarted, err := WaitForRestart(ctx, clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)
		if restarted {
			t.Fatal("WaitForRestart reported a restart on a canceled context")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitForRestart error = %v, want context.Canceled", err)
		}
		if probes.daemonCalls != 0 || len(*sleeps) != 0 {
			t.Fatalf("canceled wait polled %d times and slept %d times, want 0 and 0", probes.daemonCalls, len(*sleeps))
		}
	})
}

func TestVerifyAllGreen(t *testing.T) {
	t.Parallel()
	checks := Verify(context.Background(), greenProbes(), verifyTestWant)

	wantNames := []string{checkGTVersion, checkDaemonVersion, checkImageLabel, checkHealth}
	if len(checks) != len(wantNames) {
		t.Fatalf("got %d checks, want %d", len(checks), len(wantNames))
	}
	for i, name := range wantNames {
		if checks[i].Name != name {
			t.Errorf("check %d is %q, want %q", i, checks[i].Name, name)
		}
		if !checks[i].OK {
			t.Errorf("check %q failed: %s", checks[i].Name, checks[i].Detail)
		}
	}
}

func TestVerifyReportsEachFailure(t *testing.T) {
	t.Parallel()
	t.Run("image label", func(t *testing.T) {
		t.Parallel()
		probes := greenProbes()
		probes.image = "fffffff"

		assertOnlyFailed(t, Verify(context.Background(), probes, verifyTestWant), checkImageLabel)
	})

	t.Run("health", func(t *testing.T) {
		t.Parallel()
		probes := greenProbes()
		probes.health = "RED: drain in progress"

		assertOnlyFailed(t, Verify(context.Background(), probes, verifyTestWant), checkHealth)
	})
}

func TestVerifyProbeErrorIsAFailedCheck(t *testing.T) {
	t.Parallel()
	probes := greenProbes()
	probes.daemon = []verifyAnswer{{err: errors.New("daemon socket refused")}}

	checks := Verify(context.Background(), probes, verifyTestWant)
	if len(checks) != 4 {
		t.Fatalf("got %d checks, want 4", len(checks))
	}
	daemon := findCheck(t, checks, checkDaemonVersion)
	if daemon.OK {
		t.Fatal("daemon-version passed on a probe error")
	}
	if daemon.Detail != "daemon socket refused" {
		t.Fatalf("daemon-version detail = %q, want the probe error text", daemon.Detail)
	}
	for _, name := range []string{checkGTVersion, checkImageLabel, checkHealth} {
		if check := findCheck(t, checks, name); !check.OK {
			t.Errorf("%q was not evaluated after another probe failed: %s", name, check.Detail)
		}
	}
}

func TestSameCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"short against full", verifyTestShort, verifyTestWant, true},
		{"full against short", verifyTestWant, verifyTestShort, true},
		{"identical full", verifyTestWant, verifyTestWant, true},
		{"different builds", verifyTestOld, verifyTestShort, false},
		{"shorter than seven", "012345", verifyTestWant, false},
		{"empty", "", "", false},
		{"full against a short build it does not start with", verifyTestWant, "fedcba987", false},
		{"trailing newline from a command", verifyTestWant + "\n", verifyTestShort, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameCommit(tt.a, tt.b); got != tt.want {
				t.Fatalf("sameCommit(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// assertOnlyFailed requires exactly the named check to fail.
func assertOnlyFailed(t *testing.T, checks []Check, failed string) {
	t.Helper()
	if len(checks) != 4 {
		t.Fatalf("got %d checks, want 4", len(checks))
	}
	for _, check := range checks {
		if wantOK := check.Name != failed; check.OK != wantOK {
			t.Errorf("check %q OK = %v, want %v (detail: %s)", check.Name, check.OK, wantOK, check.Detail)
		}
	}
}

// findCheck returns the check with name, failing the test when it is absent.
func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("no check named %q in %v", name, checks)
	return Check{}
}

// parseVerifyTime returns the fixed start time the wait tests use.
func parseVerifyTime(t *testing.T) time.Time {
	t.Helper()
	start, err := time.Parse(time.RFC3339, verifyTestStart)
	if err != nil {
		t.Fatalf("parse start time: %v", err)
	}
	return start
}
