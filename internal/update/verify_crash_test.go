package update

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// crashAnswer is one PodCrashing result.
type crashAnswer struct {
	crashing bool
	why      string
	err      error
}

// crashProbes is fakeVerifyProbes with the crash probe: it answers its script
// in order and repeats the last answer once the script runs out, recording each
// call so a test can see the wait asked on every poll.
type crashProbes struct {
	*fakeVerifyProbes
	crash      []crashAnswer
	crashCalls int
}

func (p *crashProbes) PodCrashing(context.Context) (bool, string, error) {
	answer := crashAnswer{}
	if len(p.crash) > 0 {
		answer = p.crash[min(p.crashCalls, len(p.crash)-1)]
	}
	p.crashCalls++
	return answer.crashing, answer.why, answer.err
}

// TestPreRollGuardCrashingPodEndsTheWait drives the crash probe: the poll that
// finds the new pod crash-looping ends the wait there, with healthy false and
// why in the error, instead of spending the whole bound on a pod that cannot
// come up.
func TestPreRollGuardCrashingPodEndsTheWait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pods []crashAnswer
		// why is the reason the returned error must carry.
		why string
	}{
		{
			name: "crash-looping on the first poll",
			pods: []crashAnswer{{crashing: true, why: "gt: CrashLoopBackOff"}},
			why:  "gt: CrashLoopBackOff",
		},
		{
			name: "crash-looping after two polls that reported it up",
			pods: []crashAnswer{{}, {}, {crashing: true, why: "daemon: exit code 3"}},
			why:  "daemon: exit code 3",
		},
		{
			name: "crash-looping with no container reason",
			pods: []crashAnswer{{crashing: true, why: " "}},
			why:  "crash-looping",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start := parseVerifyTime(t)
			clock := &fakeVerifyClock{now: start}
			sleep, _ := verifySleeps(clock)
			probes := &crashProbes{
				fakeVerifyProbes: &fakeVerifyProbes{daemon: []verifyAnswer{{version: verifyTestOld}}},
				crash:            tt.pods,
			}

			restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)

			if restarted {
				t.Fatal("WaitForRestart reported a restart while the newest pod was crash-looping")
			}
			if err == nil {
				t.Fatal("WaitForRestart ended without an error saying why the pod is crash-looping")
			}
			if !strings.Contains(err.Error(), tt.why) {
				t.Errorf("WaitForRestart error = %q, want it to carry the reason %q", err, tt.why)
			}
			if want := time.Duration(len(tt.pods)-1) * verifyTestPoll; clock.now.Sub(start) != want {
				t.Errorf("wait ended %s after the first poll, want %s: it must end on the poll that found the pod crashing",
					clock.now.Sub(start), want)
			}
			if probes.crashCalls != len(tt.pods) {
				t.Errorf("asked the crash probe %d times, want %d: the wait must end on the poll that found the pod crashing",
					probes.crashCalls, len(tt.pods))
			}
		})
	}
}

// TestPreRollGuardDaemonReachingTheTargetBeatsACrashReading drives the poll
// order: the daemon's version is read first, so a poll that finds it on the
// target is a restart even when the crash probe, read after it, still sees the
// old pod crash-looping or cannot read it. A stale crash reading must not fail
// a restart that has already happened.
func TestPreRollGuardDaemonReachingTheTargetBeatsACrashReading(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pods []crashAnswer
	}{
		{
			name: "a crash reading on the same poll",
			pods: []crashAnswer{{crashing: true, why: "gt: CrashLoopBackOff"}},
		},
		{
			name: "a probe error on the same poll",
			pods: []crashAnswer{{err: errors.New("kubectl: no route to host")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeVerifyClock{now: parseVerifyTime(t)}
			sleep, _ := verifySleeps(clock)
			probes := &crashProbes{
				fakeVerifyProbes: &fakeVerifyProbes{daemon: []verifyAnswer{{version: verifyTestShort}}},
				crash:            tt.pods,
			}

			restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)

			if err != nil {
				t.Fatalf("WaitForRestart: %v, want the daemon on the target to end the wait", err)
			}
			if !restarted {
				t.Fatal("WaitForRestart reported no restart though the daemon reported the target")
			}
			if probes.crashCalls != 0 {
				t.Errorf("asked the crash probe %d times, want 0: the poll that found the target must not read the pod",
					probes.crashCalls)
			}
		})
	}
}

// TestPreRollGuardNoCrashLeavesTheWaitAsBefore drives the wait with no crash
// probe and with one that reports the pod up: both reach the daemon when it
// reports the target and time out otherwise, exactly as the wait did before the
// probe existed.
func TestPreRollGuardNoCrashLeavesTheWaitAsBefore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		withCrash bool
		daemon    []verifyAnswer
		// restarted is the wait's result; a false one must be the timeout,
		// returning no error.
		restarted bool
		// wantPolls is the daemon polls a restarting wait takes; 0 means the
		// wait times out and only the bound is checked.
		wantPolls int
	}{
		{
			name:      "no crash probe reaches the new daemon",
			daemon:    []verifyAnswer{{version: verifyTestOld}, {version: verifyTestShort}},
			restarted: true,
			wantPolls: 2,
		},
		{
			name:      "no crash probe times out",
			daemon:    []verifyAnswer{{version: verifyTestOld}},
			restarted: false,
		},
		{
			name:      "an up pod reaches the new daemon",
			withCrash: true,
			daemon:    []verifyAnswer{{version: verifyTestOld}, {version: verifyTestShort}},
			restarted: true,
			wantPolls: 2,
		},
		{
			name:      "an up pod times out",
			withCrash: true,
			daemon:    []verifyAnswer{{version: verifyTestOld}},
			restarted: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start := parseVerifyTime(t)
			clock := &fakeVerifyClock{now: start}
			sleep, _ := verifySleeps(clock)
			base := &fakeVerifyProbes{daemon: tt.daemon}
			var probes VerifyProbes = base
			if tt.withCrash {
				probes = &crashProbes{fakeVerifyProbes: base}
			}

			restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)

			if err != nil {
				t.Fatalf("WaitForRestart: %v", err)
			}
			if restarted != tt.restarted {
				t.Fatalf("WaitForRestart restarted = %v, want %v", restarted, tt.restarted)
			}
			if tt.restarted {
				if base.daemonCalls != tt.wantPolls {
					t.Errorf("polled the daemon %d times, want %d", base.daemonCalls, tt.wantPolls)
				}
				return
			}
			if elapsed := clock.now.Sub(start); elapsed < verifyTestTimeout || elapsed > verifyTestTimeout+verifyTestPoll {
				t.Errorf("wait ended after %s, want the timeout %s plus at most one poll", elapsed, verifyTestTimeout)
			}
		})
	}
}

// TestPreRollGuardCrashProbeErrorKeepsTheWait drives the crash probe's error: a
// pod it cannot read is reported and the wait goes on to reach the daemon, and a
// pod that is never readable still ends only at the timeout, with no error.
func TestPreRollGuardCrashProbeErrorKeepsTheWait(t *testing.T) {
	t.Parallel()
	unreadable := errors.New("kubectl: no route to host")
	tests := []struct {
		name      string
		pods      []crashAnswer
		daemon    []verifyAnswer
		restarted bool
	}{
		{
			name:      "an unreadable pod does not end the wait",
			pods:      []crashAnswer{{err: unreadable}, {err: unreadable}, {}},
			daemon:    []verifyAnswer{{version: verifyTestOld}, {version: verifyTestOld}, {version: verifyTestShort}},
			restarted: true,
		},
		{
			name:      "a pod that is never readable times out",
			pods:      []crashAnswer{{err: unreadable}},
			daemon:    []verifyAnswer{{version: verifyTestOld}},
			restarted: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start := parseVerifyTime(t)
			clock := &fakeVerifyClock{now: start}
			sleep, _ := verifySleeps(clock)
			probes := &crashProbes{
				fakeVerifyProbes: &fakeVerifyProbes{daemon: tt.daemon},
				crash:            tt.pods,
			}

			restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)

			if err != nil {
				t.Fatalf("WaitForRestart: %v, want the crash probe's error to leave the wait running", err)
			}
			if restarted != tt.restarted {
				t.Fatalf("WaitForRestart restarted = %v, want %v", restarted, tt.restarted)
			}
			// Every poll that did not see the target asks the crash probe; the
			// one that does ends the wait before the pod is read.
			wantCrashCalls := probes.daemonCalls
			if tt.restarted {
				wantCrashCalls--
			}
			if probes.crashCalls != wantCrashCalls {
				t.Errorf("asked the crash probe %d times and the daemon %d times, want %d: one crash poll per daemon poll that did not see the target",
					probes.crashCalls, probes.daemonCalls, wantCrashCalls)
			}
			if probes.crashCalls < 2 {
				t.Errorf("asked the crash probe %d times, want the wait to go on past its first failure", probes.crashCalls)
			}
			if !tt.restarted {
				if elapsed := clock.now.Sub(start); elapsed < verifyTestTimeout {
					t.Errorf("wait ended after %s, want the timeout %s", elapsed, verifyTestTimeout)
				}
			}
		})
	}
}

// TestPreRollGuardCrashProbeErrorLogIsBounded drives the log bound: a pod that
// stays unreadable is reported once for the whole wait, however many polls it
// takes, and one that fails again after a good read is reported again. The
// standard logger is redirected only to observe what the wait writes, never to
// steer it, so this test runs with the package's other tests paused.
func TestPreRollGuardCrashProbeErrorLogIsBounded(t *testing.T) {
	unreadable := errors.New("kubectl: no route to host")
	tests := []struct {
		name string
		pods []crashAnswer
		want int
	}{
		{
			name: "unreadable for the whole wait logs once",
			pods: []crashAnswer{{err: unreadable}},
			want: 1,
		},
		{
			name: "unreadable again after a good read logs again",
			pods: []crashAnswer{{err: unreadable}, {}, {err: unreadable}},
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeVerifyClock{now: parseVerifyTime(t)}
			sleep, _ := verifySleeps(clock)
			probes := &crashProbes{
				fakeVerifyProbes: &fakeVerifyProbes{daemon: []verifyAnswer{{version: verifyTestOld}}},
				crash:            tt.pods,
			}
			var logged bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(previous) })

			restarted, err := WaitForRestart(context.Background(), clock, sleep, probes, verifyTestWant, verifyTestTimeout, verifyTestPoll)

			if err != nil {
				t.Fatalf("WaitForRestart: %v, want the probe's error to leave the wait running", err)
			}
			if restarted {
				t.Fatal("WaitForRestart reported a restart though the daemon never changed")
			}
			if probes.crashCalls < 3 {
				t.Fatalf("asked the crash probe %d times, want the wait to keep polling", probes.crashCalls)
			}
			if got := strings.Count(logged.String(), "crash probe failed"); got != tt.want {
				t.Errorf("logged the crash probe's error %d times over %d polls, want %d: %q",
					got, probes.crashCalls, tt.want, logged.String())
			}
		})
	}
}

// TestPreRollGuardCrashLoopEndsTheRunFailed drives the wait's caller: a restart
// whose newest pod crash-loops is not the timeout case, so the run ends failed
// instead of partial, the restart step carries the reason, and the run never
// goes on to verify.
func TestPreRollGuardCrashLoopEndsTheRunFailed(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	why := "gt: CrashLoopBackOff"
	// The daemon never reports the new build, so every poll asks the crash
	// probe, which reports the new pod crash-looping.
	f.verify.daemon = []verifyAnswer{{version: verifyTestOld}}
	f.deps.Verify = &crashProbes{fakeVerifyProbes: f.verify, crash: []crashAnswer{{crashing: true, why: why}}}

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomeFailed {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeFailed)
	}
	restart := step(t, final, StepRestart)
	if restart.Result != ResultFailed {
		t.Errorf("restart Result = %q, want %q", restart.Result, ResultFailed)
	}
	if !strings.Contains(restart.Detail, why) {
		t.Errorf("restart Detail = %q, want it to carry the reason %q", restart.Detail, why)
	}
	if got := step(t, final, StepVerify).Result; got != ResultPending {
		t.Errorf("verify Result = %q, want %q: the run ends at the restart", got, ResultPending)
	}
	if f.holdExists() {
		t.Error("the run owns the hold, so it must have released it")
	}
}
