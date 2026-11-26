package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// throttledCPUStat is a cgroup v2 cpu.stat whose container was throttled.
const throttledCPUStat = "usage_usec 125000000\nnr_periods 480\nnr_throttled 12\nthrottled_usec 3400000\n"

// runnerPod is the one pod the lookup fakes name.
const runnerPod = "forgejo-runner-7d9f6b8c4-x2k9p"

// runnerExec answers the pod lookup with listing and the cpu.stat read with
// stat, so a fake models the seam ReadRunnerThrottle drives in two calls.
func runnerExec(listing, stat string) func(context.Context, ...string) (string, error) {
	var calls int
	return func(context.Context, ...string) (string, error) {
		calls++
		if calls == 1 {
			return listing, nil
		}
		return stat, nil
	}
}

func TestRunnerThrottleParses(t *testing.T) {
	t.Parallel()
	want := RunnerThrottle{Periods: 480, Throttled: 12, ThrottledUsec: 3400000}
	if got, err := ParseCPUStat(throttledCPUStat); err != nil || got != want {
		t.Fatalf("ParseCPUStat = %+v, %v, want %+v", got, err, want)
	}
}

// TestRunnerThrottleReadsThroughThePodByLabel: the read the recorder takes is
// the lookup of the runner pod by label, then the cpu.stat read inside that
// pod's dind container, and the counters it parses come back. No argument
// names a deployment: that get is what the sweep-runner role refuses.
func TestRunnerThrottleReadsThroughThePodByLabel(t *testing.T) {
	t.Parallel()
	var calls [][]string
	exec := func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		if len(calls) == 1 {
			return "pod/" + runnerPod + "\n", nil
		}
		return throttledCPUStat, nil
	}
	got, err := ReadRunnerThrottle(context.Background(), exec)
	if err != nil {
		t.Fatalf("ReadRunnerThrottle: %v", err)
	}
	if want := (RunnerThrottle{Periods: 480, Throttled: 12, ThrottledUsec: 3400000}); got != want {
		t.Errorf("ReadRunnerThrottle = %+v, want the pod's counters %+v", got, want)
	}
	want := []string{
		"get pods -n forgejo -l app.kubernetes.io/name=forgejo-runner -o name",
		"-n forgejo exec " + runnerPod + " -c dind -- cat /sys/fs/cgroup/cpu.stat",
	}
	if len(calls) != len(want) {
		t.Fatalf("ReadRunnerThrottle ran %d kubectl call(s), want %d: %q", len(calls), len(want), calls)
	}
	for i := range want {
		if got := strings.Join(calls[i], " "); got != want[i] {
			t.Errorf("call %d ran %q, want %q", i+1, got, want[i])
		}
	}
	if i := slices.Index(calls[1], "-c"); i < 0 || i+1 >= len(calls[1]) || calls[1][i+1] != "dind" {
		t.Errorf("the read does not exec into the dind container: %q", calls[1])
	}
	for _, args := range calls {
		for _, arg := range args {
			if strings.Contains(arg, "deploy/") {
				t.Errorf("ReadRunnerThrottle ran %q, an argument naming a deployment", strings.Join(args, " "))
			}
		}
	}
}

// readError answers the pod lookup with one pod, then fails the read.
func readError(err error) func(context.Context, ...string) (string, error) {
	var calls int
	return func(context.Context, ...string) (string, error) {
		calls++
		if calls == 1 {
			return "pod/" + runnerPod + "\n", nil
		}
		return "", err
	}
}

// lookupError fails the pod lookup, the read's first call.
func lookupError(err error) func(context.Context, ...string) (string, error) {
	return func(context.Context, ...string) (string, error) { return "", err }
}

func TestRunnerThrottleErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		read bool
		exec func(context.Context, ...string) (string, error)
		want string
	}{
		{"a missing counter", "nr_periods 1\nnr_throttled 1\n", false, nil, "carries no throttled_usec"},
		{"an empty answer", "", true, runnerExec("pod/"+runnerPod+"\n", ""), "carries no nr_periods"},
		{"a read error", "", true, readError(errors.New("kubectl is unreachable")), "kubectl is unreachable"},
		{"a lookup error", "", true, lookupError(errors.New("kubectl is unreachable")), "kubectl is unreachable"},
		{"no runner pod", "", true, runnerExec("", throttledCPUStat), "found 0 runner pods"},
		{"two runner pods", "", true, runnerExec("pod/forgejo-runner-a\npod/forgejo-runner-b\n", throttledCPUStat), "found 2 runner pods"},
		{"no exec seam", "", true, nil, "no exec seam"},
		{"a value that is not a count", "nr_periods soon\n", false, nil, "nr_periods is not a count"},
		{"a negative counter", "nr_periods -5\n", false, nil, "nr_periods is not a count"},
		{"a counter named twice", "nr_periods 1\nnr_periods 2\n", false, nil, "names nr_periods twice"},
		{"a counter with two values", "nr_periods 1 2\n", false, nil, "nr_periods is not a count"},
	}
	for _, c := range cases {
		var err error
		if c.read {
			_, err = ReadRunnerThrottle(context.Background(), c.exec)
		} else {
			_, err = ParseCPUStat(c.text)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error naming %q", c.name, err, c.want)
		}
	}
}

// A seam that edits the argv it is handed must not change what the next read
// sends: each call hands the seam a fresh argv, never one the seam can reach.
func TestReadRunnerThrottleArgvIsCopied(t *testing.T) {
	t.Parallel()
	want := []string{
		"get pods -n forgejo -l app.kubernetes.io/name=forgejo-runner -o name",
		"-n forgejo exec " + runnerPod + " -c dind -- cat /sys/fs/cgroup/cpu.stat",
	}
	var sent []string
	exec := func(_ context.Context, args ...string) (string, error) {
		sent = append(sent, strings.Join(args, " "))
		if len(sent)%2 == 1 {
			args[len(args)-1] = "corrupted"
			return "pod/" + runnerPod + "\n", nil
		}
		args[len(args)-1] = "corrupted"
		return throttledCPUStat, nil
	}
	for range 2 {
		if _, err := ReadRunnerThrottle(context.Background(), exec); err != nil {
			t.Fatalf("ReadRunnerThrottle: %v", err)
		}
	}
	for i, wantOne := range slices.Concat(want, want) {
		if sent[i] != wantOne {
			t.Errorf("call %d sent %q, want %q", i+1, sent[i], wantOne)
		}
	}
}

// podsAnswer is an exec seam whose kubectl listing prints out and succeeds.
func podsAnswer(out string) func(context.Context, ...string) (string, error) {
	return func(context.Context, ...string) (string, error) { return out, nil }
}

func TestRunnerPodNameReturnsTheOnePod(t *testing.T) {
	t.Parallel()
	var argv []string
	got, err := runnerPodName(context.Background(), func(_ context.Context, args ...string) (string, error) {
		argv = args
		return "pod/forgejo-runner-7d9f6b8c4-x2k9p\n", nil
	})
	if err != nil || got != "forgejo-runner-7d9f6b8c4-x2k9p" {
		t.Fatalf("runnerPodName = %q, %v, want the one pod name", got, err)
	}
	want := "get pods -n forgejo -l app.kubernetes.io/name=forgejo-runner -o name"
	if gotArgs := strings.Join(argv, " "); gotArgs != want {
		t.Fatalf("runnerPodName ran %q, want %q", gotArgs, want)
	}
}

func TestRunnerPodNameErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		exec func(context.Context, ...string) (string, error)
		want string
	}{
		{"no pod", podsAnswer(""), "found 0 runner pods"},
		{"two pods", podsAnswer("pod/forgejo-runner-a\npod/forgejo-runner-b\n"), "found 2 runner pods"},
		{"no exec seam", nil, "no exec seam"},
		{"a lookup error", func(context.Context, ...string) (string, error) { return "", errors.New("kubectl is unreachable") }, "kubectl is unreachable"},
	}
	for _, c := range cases {
		_, err := runnerPodName(context.Background(), c.exec)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error naming %q", c.name, err, c.want)
		}
	}
}
