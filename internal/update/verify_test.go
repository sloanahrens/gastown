package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townhealth"
)

const (
	verifyTestWant         = "0123456789abcdef0123456789abcdef01234567" // a 40-character main sha
	verifyTestShort        = "012345678"                                // the 9 characters gt version prints
	verifyTestOld          = "fedcba987"                                // a build that is not the target
	verifyTestImage        = "0123456"
	verifyTestPolecat      = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	verifyTestOtherPolecat = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	verifyTestGreen        = "GREEN tick 2m ago"
	verifyTestSettling     = "DEGRADED tick 20s ago: tick:overrun_guard=2m/1m[R]"
	verifyTestDeadSeat     = "DEGRADED tick 20s ago: seat/gastown:polecat/nux=dead[R]"
	verifyTestStart        = "2026-11-03T12:00:00Z"
	verifyTestPoll         = 15 * time.Second
	verifyTestTimeout      = time.Minute
)

// verifyAnswer is one DaemonVersion result.
type verifyAnswer struct {
	version string
	err     error
}

// fakeVerifyProbes answers each probe with what a test set, recording calls.
// The daemon answers its script in order and repeats the last answer once the
// script runs out; healthLines is a health script read the same way.
type fakeVerifyProbes struct {
	gt, image, health                             string
	errGT, errImage, errHealth                    error
	daemon                                        []verifyAnswer
	healthLines                                   []string
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
	if len(p.healthLines) > 0 {
		return p.healthLines[min(p.healthCalls-1, len(p.healthLines)-1)], nil
	}
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

// fakePairProbes is fakeVerifyProbes with the pair extension: the wanted
// polecat digest and the two records the pair checks read.
type fakePairProbes struct {
	*fakeVerifyProbes
	wantPolecat              string
	annotation, devTag       string
	errAnnotation, errDevTag error
}

func (p *fakePairProbes) WantPolecat() string { return p.wantPolecat }

func (p *fakePairProbes) PolecatAnnotation(context.Context) (string, error) {
	return p.annotation, p.errAnnotation
}

func (p *fakePairProbes) DevTagDigest(context.Context) (string, error) {
	return p.devTag, p.errDevTag
}

// pairGreenProbes is every probe matching want and the pair on its digest.
func pairGreenProbes() *fakePairProbes {
	return &fakePairProbes{
		fakeVerifyProbes: greenProbes(),
		wantPolecat:      verifyTestPolecat,
		annotation:       verifyTestPolecat,
		devTag:           verifyTestPolecat,
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

// settleProbes runs VerifyWithSettle on probes whose health answers lines, with
// wait as the bound, and returns the checks and the sleeps it recorded.
func settleProbes(t *testing.T, probes *fakeVerifyProbes, wait time.Duration, lines ...string) ([]Check, *[]time.Duration) {
	t.Helper()
	probes.healthLines = lines
	clock := &fakeVerifyClock{now: parseVerifyTime(t)}
	sleep, sleeps := verifySleeps(clock)
	checks, err := VerifyWithSettle(context.Background(), clock, sleep, probes, verifyTestWant, wait, verifyTestPoll)
	if err != nil {
		t.Fatalf("VerifyWithSettle: %v", err)
	}
	return checks, sleeps
}

// settleVerify runs VerifyWithSettle on green probes answering lines.
func settleVerify(t *testing.T, wait time.Duration, lines ...string) ([]Check, *[]time.Duration) {
	t.Helper()
	return settleProbes(t, greenProbes(), wait, lines...)
}

// TestVerifyWithSettlePassesWhenTheLateTickClears: two settling reads then
// GREEN pass, and the check names the settle time.
func TestVerifyWithSettlePassesWhenTheLateTickClears(t *testing.T) {
	t.Parallel()
	checks, _ := settleVerify(t, verifyTestTimeout, verifyTestSettling, verifyTestSettling, verifyTestGreen)
	assertFailedChecks(t, checks)
	if health := findCheck(t, checks, checkHealth); health.Detail != verifyTestGreen+" (settled after 30s)" {
		t.Errorf("health detail = %q, want the settled line and its settle time", health.Detail)
	}
}

// TestVerifyWithSettleSettlesWhenADeadSeatClears: a recorded dead seat that
// clears on a later read passes, and the check names the settle time.
func TestVerifyWithSettleSettlesWhenADeadSeatClears(t *testing.T) {
	t.Parallel()
	checks, _ := settleVerify(t, verifyTestTimeout, verifyTestDeadSeat, verifyTestGreen)
	assertFailedChecks(t, checks)
	if health := findCheck(t, checks, checkHealth); health.Detail != verifyTestGreen+" (settled after 15s)" {
		t.Errorf("health detail = %q, want the settled line and its settle time", health.Detail)
	}
}

// TestVerifyWithSettleStillDeadPastTheBoundIsPartialNamingTheSeat: a seat that
// stays dead past the bound is a partial, and the detail names the seat so the
// escalation says which seat to clear.
func TestVerifyWithSettleStillDeadPastTheBoundIsPartialNamingTheSeat(t *testing.T) {
	t.Parallel()
	checks, sleeps := settleVerify(t, 5*time.Minute, verifyTestDeadSeat)

	assertFailedChecks(t, checks, checkHealth)
	if health := findCheck(t, checks, checkHealth); health.Detail != verifyTestDeadSeat+" (still settling after 5m0s)" {
		t.Errorf("health detail = %q, want the seat line as read and the bound", health.Detail)
	}
	if len(*sleeps) != 20 {
		t.Errorf("slept %d times, want 20: one per poll inside the 5m bound", len(*sleeps))
	}
}

// TestVerifyWithSettleFailsAtTheWaitBound: a line that stays settling fails at
// the bound, carrying the last line read.
func TestVerifyWithSettleFailsAtTheWaitBound(t *testing.T) {
	t.Parallel()
	checks, sleeps := settleVerify(t, verifyTestTimeout, verifyTestSettling)

	assertFailedChecks(t, checks, checkHealth)
	if health := findCheck(t, checks, checkHealth); health.Detail != verifyTestSettling+" (still settling after 1m0s)" {
		t.Errorf("health detail = %q, want the last line read and the bound", health.Detail)
	}
	if len(*sleeps) != 4 {
		t.Errorf("slept %d times, want 4: one per poll inside the bound", len(*sleeps))
	}
}

// TestVerifyWithSettleReturnsAFailedSleepError: a failed sleep is not swallowed.
func TestVerifyWithSettleReturnsAFailedSleepError(t *testing.T) {
	t.Parallel()
	probes := greenProbes()
	probes.healthLines = []string{verifyTestSettling}
	sleep := func(context.Context, time.Duration) error { return errors.New("canceled") }
	clock := &fakeVerifyClock{now: parseVerifyTime(t)}
	_, err := VerifyWithSettle(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)
	if err == nil || err.Error() != "canceled" {
		t.Fatalf("VerifyWithSettle error = %v, want the sleep's", err)
	}
}

// TestVerifyWithSettleFailsAtOnceOnARedLine: a RED line is a fault, not a
// settle, so it is read once and not waited on.
func TestVerifyWithSettleFailsAtOnceOnARedLine(t *testing.T) {
	t.Parallel()
	const red = "RED tick 1m ago: landing=stuck"
	probes := greenProbes()
	checks, sleeps := settleProbes(t, probes, verifyTestTimeout, red)

	assertFailedChecks(t, checks, checkHealth)
	if health := findCheck(t, checks, checkHealth); health.Detail != red {
		t.Errorf("health detail = %q, want the red line as read", health.Detail)
	}
	if probes.healthCalls != 1 {
		t.Errorf("read the health line %d times, want 1: a red line must not be read again", probes.healthCalls)
	}
	if len(*sleeps) != 0 {
		t.Errorf("slept %d times, want none: a red line is not a settle", len(*sleeps))
	}
}

// TestVerifyWithSettleDoesNotRetryAProbeError: a health probe that errors is
// not read again and does not wait.
func TestVerifyWithSettleDoesNotRetryAProbeError(t *testing.T) {
	t.Parallel()
	probes := greenProbes()
	probes.errHealth = errors.New("health probe timed out")
	checks, sleeps := settleProbes(t, probes, verifyTestTimeout)

	assertFailedChecks(t, checks, checkHealth)
	if health := findCheck(t, checks, checkHealth); health.Detail != "health probe timed out" {
		t.Errorf("health detail = %q, want the probe's error", health.Detail)
	}
	if probes.healthCalls != 1 {
		t.Errorf("read the health line %d times, want 1: a probe error is not retried", probes.healthCalls)
	}
	if len(*sleeps) != 0 {
		t.Errorf("slept %d times, want none: a probe error is not waited on", len(*sleeps))
	}
}

// TestVerifyWithSettleKeepsTheVersionChecksFromTheFirstRun: the settle re-reads
// only the health line, so the version and image probes are asked once.
func TestVerifyWithSettleKeepsTheVersionChecksFromTheFirstRun(t *testing.T) {
	t.Parallel()
	probes := greenProbes()
	checks, _ := settleProbes(t, probes, verifyTestTimeout, verifyTestSettling, verifyTestGreen)

	assertFailedChecks(t, checks)
	if probes.gtCalls != 1 || probes.daemonCalls != 1 || probes.imageCalls != 1 {
		t.Errorf("version and image probes called %d, %d and %d times, want 1 each: the settle re-reads only the health line",
			probes.gtCalls, probes.daemonCalls, probes.imageCalls)
	}
	if probes.healthCalls != 2 {
		t.Errorf("read the health line %d times, want 2: the first read and one settling re-read", probes.healthCalls)
	}
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

// assertHealthClass requires every line to classify as want.
func assertHealthClass(t *testing.T, lines []string, want healthClass) {
	t.Helper()
	for _, line := range lines {
		if got := classifyHealthLine(line); got != want {
			t.Errorf("classifyHealthLine(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestClassifyHealthLinePassesGreenAndQueueOnlyLines drives classifyHealthLine
// on the lines a roll passes on today: a GREEN line, and a DEGRADED one whose
// every reason is a queue a person works through, as queueOnlyHealthLine reads
// them.
func TestClassifyHealthLinePassesGreenAndQueueOnlyLines(t *testing.T) {
	t.Parallel()
	assertHealthClass(t, []string{
		"GREEN tick 2m ago",
		"GREEN tick 2m ago, 3 landed/24h",
		"DEGRADED tick 28s ago: needs-overseer=1",
		"DEGRADED tick 4ms ago: escalation=oldest_2h needs-human=1",
		"DEGRADED tick 5s ago: needs-overseer=1, needs-human=2, escalation=oldest_2h",
	}, healthPass)
}

// TestClassifyHealthLineSettlesOnARecordedLateTick drives classifyHealthLine
// on the line the 2026-11-19 roll recorded: a patrol late against its interval
// until its first pass on the new daemon, alone and beside a queue state.
func TestClassifyHealthLineSettlesOnARecordedLateTick(t *testing.T) {
	t.Parallel()
	assertHealthClass(t, []string{
		"DEGRADED tick 20s ago: tick:overrun_guard=2m/1m[R]",
		"DEGRADED tick 20s ago: tick:overrun_guard=2m/1m[R] needs-human=1",
		"DEGRADED tick 6m ago: tick:wisp_reaper=70m/30m[R], tick:overrun_guard=2m/1m[R]",
	}, healthSettling)
}

// TestClassifyHealthLineSettlesARecordedDeadSeat drives classifyHealthLine on
// the line the 2026-11-20 roll recorded: a polecat seat dead in consecutive
// samples while it was re-slung onto another bead, alone, beside a queue state
// and beside a late tick.
func TestClassifyHealthLineSettlesARecordedDeadSeat(t *testing.T) {
	t.Parallel()
	assertHealthClass(t, []string{
		"DEGRADED tick 20s ago: seat/gastown:polecat/nux=dead[R]",
		"DEGRADED tick 20s ago: seat/gastown:polecat/nux=dead[R] needs-human=1",
		"DEGRADED tick 6m ago: seat/gastown:polecat/nux=dead[R], tick:overrun_guard=2m/1m[R]",
	}, healthSettling)
}

// TestClassifyHealthLineFailsASeatReasonThatIsNotARecordedDeadOne drives
// classifyHealthLine on seat reasons that are not a recorded dead polecat: a
// frozen seat, a stalled one, a dead seat without the recorded tag, and a dead
// seat beside a reason this does not know.
func TestClassifyHealthLineFailsASeatReasonThatIsNotARecordedDeadOne(t *testing.T) {
	t.Parallel()
	assertHealthClass(t, []string{
		"DEGRADED tick 20s ago: seat/gastown:polecat/nux=frozen[R]",
		"DEGRADED tick 20s ago: seat/gastown:polecat/nux=stalled_12m[R]",
		"DEGRADED tick 20s ago: seat/gastown:polecat/nux=dead",
		"DEGRADED tick 20s ago: seat/gastown:polecat/nux=dead[R] main/gastown=behind",
		"DEGRADED tick 20s ago: seat/gastown:polecat/nux=dead[R]x",
		"DEGRADED tick 20s ago: seat/gastown:crew/nux=dead[R]",
		"DEGRADED tick 20s ago: seat/gastown:polecat/=dead[R]",
		"DEGRADED tick 20s ago: seat/:polecat/nux=dead[R]",
	}, healthFail)
}

// TestClassifyHealthLineFailsARedLine drives classifyHealthLine on the
// verdicts that are no settling state: a RED line alone, beside a queue state
// and beside a late tick, and a stale UNKNOWN line.
func TestClassifyHealthLineFailsARedLine(t *testing.T) {
	t.Parallel()
	assertHealthClass(t, []string{
		"RED tick 1m ago: landing=stuck",
		"RED tick 1m ago: needs-overseer=1",
		"RED tick 1m ago: tick:overrun_guard=2m/1m[R]",
		"UNKNOWN tick 3d ago (stale; last GREEN)",
	}, healthFail)
}

// TestClassifyHealthLineFailsATickReasonWithoutTheRecordedTag drives
// classifyHealthLine on tick tokens the town does not render: no tag, an
// UNKNOWN tag, no patrol, and a value that is not "<age>/<interval>" in the
// town's short durations.
func TestClassifyHealthLineFailsATickReasonWithoutTheRecordedTag(t *testing.T) {
	t.Parallel()
	assertHealthClass(t, []string{
		"DEGRADED tick 20s ago: tick:overrun_guard=2m/1m",
		"DEGRADED tick 20s ago: tick:overrun_guard=2m/1m[?]",
		"DEGRADED tick 20s ago: tick:overrun_guard=2m[R]",
		"DEGRADED tick 20s ago: tick:overrun_guard=2m/soon[R]",
		"DEGRADED tick 20s ago: tick:overrun_guard=1h30m/1m[R]",
		"DEGRADED tick 20s ago: tick:=2m/1m[R]",
	}, healthFail)
}

// TestClassifyHealthLineFailsAnUnknownReasonBesideATick drives
// classifyHealthLine on a late tick beside a reason that is neither a queue
// state nor a tick, and on a DEGRADED line with no reason at all.
func TestClassifyHealthLineFailsAnUnknownReasonBesideATick(t *testing.T) {
	t.Parallel()
	assertHealthClass(t, []string{
		"DEGRADED tick 20s ago: tick:overrun_guard=2m/1m[R] main/gastown=behind",
		"DEGRADED tick 20s ago: promote/gastown=3_behind[R] tick:overrun_guard=2m/1m[R]",
		"DEGRADED tick 20s ago: tick:overrun_guard=2m/1m[R] needs-overseer=lots",
		"DEGRADED tick 20s ago",
		"DEGRADED tick 20s ago:",
	}, healthFail)
}

// TestClassifyHealthLineFailsALineWithoutReasonsOrHead drives
// classifyHealthLine on lines that do not read "<VERDICT> tick <age> ago": no
// verdict, no tick word directly after the verdict, no age or no "ago", and a
// DEGRADED line whose reasons carry no colon, so a shape the parser does not
// know fails.
func TestClassifyHealthLineFailsALineWithoutReasonsOrHead(t *testing.T) {
	t.Parallel()
	assertHealthClass(t, []string{
		"",
		"GREEN",
		"GREEN 2m ago",
		"GREEN lately tick 2m ago",
		"GREEN tick 2m",
		"GREEN tick  ago",
		"DEGRADED",
		"DEGRADED tick 20s",
		"DEGRADED tick 20s ago needs-human=1",
		"tick 2m ago",
		"needs-human=1",
	}, healthFail)
}

// TestVerifyHealthIsAQueueStateNotAFailedRoll drives Verify: a town whose line
// is DEGRADED only for a queue a person works through is not a failed roll, so
// the health check passes and names the reasons it ignored; every other line
// fails and the detail is the line as it was read.
func TestVerifyHealthIsAQueueStateNotAFailedRoll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		// ignored are the queue reasons a passing check must name in its detail.
		ignored []string
	}{
		{
			name: "green",
			line: "GREEN tick 2m ago, 3 landed/24h",
		},
		{
			name: "green without a landing count",
			line: "GREEN tick 2m ago",
		},
		{
			name:    "degraded for one waiting bead",
			line:    "DEGRADED tick 28s ago: needs-overseer=1",
			ignored: []string{"needs-overseer=1"},
		},
		{
			name:    "degraded for every queue, comma separated as the bead writes it",
			line:    "DEGRADED tick 5s ago: needs-overseer=1, needs-human=2, escalation=oldest_2h",
			ignored: []string{"needs-overseer=1", "needs-human=2", "escalation=oldest_2h"},
		},
		{
			name:    "degraded for an open escalation, the line internal/townhealth renders",
			line:    "DEGRADED tick 5s ago: escalation=oldest_2h",
			ignored: []string{"escalation=oldest_2h"},
		},
		{
			name:    "degraded for a queue and an open escalation, space separated as the line renders it",
			line:    "DEGRADED tick 4ms ago: escalation=oldest_2h needs-human=1",
			ignored: []string{"escalation=oldest_2h", "needs-human=1"},
		},
		{
			name:    "degraded for a bead waiting on the operator, the third person label the town writes",
			line:    "DEGRADED tick 28s ago: needs-operator=2",
			ignored: []string{"needs-operator=2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			probes := greenProbes()
			probes.health = tt.line

			checks := Verify(context.Background(), probes, verifyTestWant)

			assertFailedChecks(t, checks)
			detail := findCheck(t, checks, checkHealth).Detail
			if !strings.Contains(detail, tt.line) {
				t.Errorf("health detail %q does not carry the line as read %q", detail, tt.line)
			}
			for _, reason := range tt.ignored {
				if !strings.Contains(detail, reason) {
					t.Errorf("health detail %q does not name the ignored reason %q", detail, reason)
				}
			}
			if len(tt.ignored) > 0 && !strings.Contains(detail, "ignored "+strings.Join(tt.ignored, ", ")) {
				t.Errorf("health detail %q does not say it ignored the queue reasons %v", detail, tt.ignored)
			}
		})
	}
}

// TestVerifyHealthPassesTheLineTheTownRenders drives Verify on the lines the
// producer writes: a report whose only non-green field is a queue state,
// rendered by internal/townhealth.Line. The parser's fixtures are copies of
// those shapes, so this ties them to the producer rather than to themselves,
// and the table covers every key the producer renders as a queue state — each
// person label and the escalation field.
func TestVerifyHealthPassesTheLineTheTownRenders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		field  townhealth.Field
		reason string
	}{
		{
			name: "open escalation",
			field: townhealth.Field{
				Name:    townhealth.FieldEscalation,
				Tag:     townhealth.Live,
				Verdict: townhealth.Degraded,
				Value:   "oldest " + townhealth.Short(2*time.Hour),
			},
			reason: "escalation=oldest_2h",
		},
		{
			name: "bead waiting on the operator",
			field: townhealth.Field{
				Name:    townhealth.FieldNeedsOperator,
				Tag:     townhealth.Live,
				Verdict: townhealth.Degraded,
				Value:   "2",
			},
			reason: "needs-operator=2",
		},
		{
			name: "bead waiting on the overseer",
			field: townhealth.Field{
				Name:    townhealth.FieldNeedsOverseer,
				Tag:     townhealth.Live,
				Verdict: townhealth.Degraded,
				Value:   "1",
			},
			reason: "needs-overseer=1",
		},
		{
			name: "bead waiting on a human",
			field: townhealth.Field{
				Name:    townhealth.FieldNeedsHuman,
				Tag:     townhealth.Live,
				Verdict: townhealth.Degraded,
				Value:   "3",
			},
			reason: "needs-human=3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			report := townhealth.Report{
				At:      now.Add(-2 * time.Minute),
				Verdict: townhealth.Degraded,
				Fields:  []townhealth.Field{tt.field},
			}
			line := townhealth.Line(report, now, 5*time.Minute)
			if !strings.Contains(line, tt.reason) {
				t.Fatalf("producer line %q does not carry the reason this test pins", line)
			}
			probes := greenProbes()
			probes.health = line

			checks := Verify(context.Background(), probes, verifyTestWant)

			assertFailedChecks(t, checks)
			detail := findCheck(t, checks, checkHealth).Detail
			if !strings.Contains(detail, "ignored "+tt.reason) {
				t.Errorf("health detail %q does not say it ignored the producer's %s reason", detail, tt.name)
			}
		})
	}
}

// TestVerifyHealthRefusesEveryOtherLine drives the refusal branches of the
// health check: any verdict other than GREEN or DEGRADED, a DEGRADED line with
// a reason that is not a queue state (a key outside queueReasons, a value that
// is not the one its field writes) or with none at all, and a line whose shape
// is not the one the town writes each fail, carrying the line as read.
func TestVerifyHealthRefusesEveryOtherLine(t *testing.T) {
	t.Parallel()
	lines := []struct {
		name string
		line string
	}{
		{"red for a landing fault", "RED tick 1m ago: landing=stuck"},
		{"red even for a queue state", "RED tick 1m ago: needs-overseer=1"},
		{"red without the tick head", "RED: drain in progress"},
		{"unknown for a stale report", "UNKNOWN tick 3d ago (stale; last GREEN)"},
		{"unknown even for a queue state", "UNKNOWN tick 3d ago: needs-human=1"},
		{"degraded for a promote lag", "DEGRADED tick 1m ago: promote/gastown=3_behind[R]"},
		{"degraded for a queue and a fault", "DEGRADED tick 2m ago: needs-overseer=1, main=behind"},
		{"degraded for an unknown queue field", "DEGRADED tick 2m ago: needs-overseer[?]"},
		{"degraded for a queue value that is not a count", "DEGRADED tick 2m ago: needs-overseer=lots"},
		{"degraded for an operator wait that is not a count", "DEGRADED tick 2m ago: needs-operator=soon"},
		{"degraded for a queue with an empty value", "DEGRADED tick 2m ago: needs-overseer="},
		{"degraded for a queue with a signed count", "DEGRADED tick 2m ago: needs-overseer=-1"},
		{"degraded for a queue carrying a tag marker", "DEGRADED tick 2m ago: needs-overseer=1[R]"},
		{"degraded for an escalation given as a count, not an age", "DEGRADED tick 2m ago: escalation=1"},
		{"degraded for an escalation age without the oldest prefix", "DEGRADED tick 2m ago: escalation=2h"},
		{"degraded for an escalation age with no unit", "DEGRADED tick 2m ago: escalation=oldest_2"},
		{"degraded for an escalation age with no number", "DEGRADED tick 2m ago: escalation=oldest_h"},
		{"degraded for an escalation age that is not a short duration", "DEGRADED tick 2m ago: escalation=oldest_lots"},
		{"degraded for the plural escalation key the town never writes", "DEGRADED tick 2m ago: escalations=1"},
		{"degraded with no reason", "DEGRADED tick 2m ago"},
		{"degraded with an empty reason list", "DEGRADED tick 2m ago:"},
		{"degraded beside a landing pause, a shape verify does not read", "DEGRADED tick 1m ago (landings paused: 2d): needs-overseer=1"},
		{"degraded without the tick head", "DEGRADED: needs-overseer=1"},
		{"degraded with no age", "DEGRADED tick  ago: needs-overseer=1"},
		{"degraded without an age token", "DEGRADED tick ago: needs-overseer=1"},
		{"verdict and nothing else", "GREEN"},
		{"not a health line", "not a health line"},
		{"empty", ""},
	}
	for _, tt := range lines {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			probes := greenProbes()
			probes.health = tt.line

			checks := Verify(context.Background(), probes, verifyTestWant)

			assertOnlyFailed(t, checks, checkHealth)
			if detail := findCheck(t, checks, checkHealth).Detail; detail != tt.line {
				t.Errorf("health detail = %q, want the line as read %q", detail, tt.line)
			}
		})
	}
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

// verifyFourNames are the checks every probe set gets.
var verifyFourNames = []string{checkGTVersion, checkDaemonVersion, checkImageLabel, checkHealth}

// verifyPairNames are the checks a pair probe set gets, in order.
var verifyPairNames = []string{
	checkGTVersion, checkDaemonVersion, checkImageLabel, checkHealth,
	checkPolecatAnnotation, checkDevTag,
}

func TestVerifyPairProbesAddsTwoChecks(t *testing.T) {
	t.Parallel()
	probes := pairGreenProbes()

	checks := Verify(context.Background(), probes, verifyTestWant)

	assertCheckNames(t, checks, verifyPairNames)
	assertFailedChecks(t, checks)
	for _, name := range []string{checkPolecatAnnotation, checkDevTag} {
		if detail := findCheck(t, checks, name).Detail; detail != verifyTestPolecat {
			t.Errorf("check %q detail = %q, want the polecat digest %q", name, detail, verifyTestPolecat)
		}
	}
}

func TestVerifyPairProbeFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*fakePairProbes)
		failed []string
		detail string
	}{
		{
			name:   "annotation mismatch",
			mutate: func(p *fakePairProbes) { p.annotation = verifyTestOtherPolecat },
			failed: []string{checkPolecatAnnotation},
			detail: fmt.Sprintf("got %q, want %q", verifyTestOtherPolecat, verifyTestPolecat),
		},
		{
			name:   "dev tag mismatch",
			mutate: func(p *fakePairProbes) { p.devTag = verifyTestOtherPolecat },
			failed: []string{checkDevTag},
			detail: fmt.Sprintf("got %q, want %q", verifyTestOtherPolecat, verifyTestPolecat),
		},
		{
			name:   "annotation probe error",
			mutate: func(p *fakePairProbes) { p.errAnnotation = errors.New("no polecat-image annotation") },
			failed: []string{checkPolecatAnnotation},
			detail: "no polecat-image annotation",
		},
		{
			name:   "dev tag probe error",
			mutate: func(p *fakePairProbes) { p.errDevTag = errors.New("the dev tag names no digest") },
			failed: []string{checkDevTag},
			detail: "the dev tag names no digest",
		},
		{
			name:   "annotation is a prefix of the digest",
			mutate: func(p *fakePairProbes) { p.annotation = verifyTestPolecat[:20] },
			failed: []string{checkPolecatAnnotation},
			detail: fmt.Sprintf("got %q, want %q", verifyTestPolecat[:20], verifyTestPolecat),
		},
		{
			name:   "no wanted digest",
			mutate: func(p *fakePairProbes) { p.wantPolecat = " " },
			failed: []string{checkPolecatAnnotation, checkDevTag},
			detail: "the pair named no polecat digest",
		},
		{
			name:   "annotation carries a trailing newline",
			mutate: func(p *fakePairProbes) { p.annotation = verifyTestPolecat + "\n" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			probes := pairGreenProbes()
			tt.mutate(probes)

			checks := Verify(context.Background(), probes, verifyTestWant)

			assertCheckNames(t, checks, verifyPairNames)
			assertFailedChecks(t, checks, tt.failed...)
			for _, name := range tt.failed {
				if detail := findCheck(t, checks, name).Detail; detail != tt.detail {
					t.Errorf("check %q detail = %q, want %q", name, detail, tt.detail)
				}
			}
		})
	}
}

func TestVerifyWithoutPairProbesChecksFour(t *testing.T) {
	t.Parallel()
	checks := Verify(context.Background(), greenProbes(), verifyTestWant)

	assertCheckNames(t, checks, verifyFourNames)
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
		{"six characters equal to themselves", "abc123", "abc123", false},
		{"one side empty", "", verifyTestWant, false},
		{"only whitespace", "       ", "       ", false},
		{"seven-character prefix", "0123456", verifyTestWant, true},
		{"whitespace around both", "  " + verifyTestShort + "\n", "\t" + verifyTestWant + " ", true},
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

// assertCheckNames requires checks to name exactly want, in order.
func assertCheckNames(t *testing.T, checks []Check, want []string) {
	t.Helper()
	if len(checks) != len(want) {
		t.Fatalf("got %d checks, want %d", len(checks), len(want))
	}
	for i, name := range want {
		if checks[i].Name != name {
			t.Errorf("check %d is %q, want %q", i, checks[i].Name, name)
		}
	}
}

// assertFailedChecks requires exactly the named checks to fail and every other
// check to pass.
func assertFailedChecks(t *testing.T, checks []Check, failed ...string) {
	t.Helper()
	wantFail := make(map[string]bool, len(failed))
	for _, name := range failed {
		wantFail[name] = true
		findCheck(t, checks, name)
	}
	for _, check := range checks {
		if check.OK == wantFail[check.Name] {
			t.Errorf("check %q OK = %v, want %v (detail: %s)", check.Name, check.OK, !wantFail[check.Name], check.Detail)
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
