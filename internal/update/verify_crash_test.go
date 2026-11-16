package update

import (
	"context"
	"errors"
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
		// daemon is the version script the wait reads; empty leaves it on
		// verifyTestOld, a build that is not the target.
		daemon []verifyAnswer
		pods   []crashAnswer
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
		{
			name:   "crash-looping on the poll the daemon reports the target",
			daemon: []verifyAnswer{{version: verifyTestShort}},
			pods:   []crashAnswer{{crashing: true, why: "gt: CrashLoopBackOff"}},
			why:    "gt: CrashLoopBackOff",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			daemon := tt.daemon
			if len(daemon) == 0 {
				daemon = []verifyAnswer{{version: verifyTestOld}}
			}
			start := parseVerifyTime(t)
			clock := &fakeVerifyClock{now: start}
			sleep, _ := verifySleeps(clock)
			probes := &crashProbes{
				fakeVerifyProbes: &fakeVerifyProbes{daemon: daemon},
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
			if probes.crashCalls != probes.daemonCalls {
				t.Errorf("asked the crash probe %d times and the daemon %d times, want one crash poll per daemon poll",
					probes.crashCalls, probes.daemonCalls)
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
