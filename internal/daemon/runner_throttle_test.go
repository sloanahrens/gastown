package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// throttledCPUStat is a cgroup v2 cpu.stat whose container was throttled.
const throttledCPUStat = "usage_usec 125000000\nnr_periods 480\nnr_throttled 12\nthrottled_usec 3400000\n"

func TestRunnerThrottleParses(t *testing.T) {
	t.Parallel()
	want := RunnerThrottle{Periods: 480, Throttled: 12, ThrottledUsec: 3400000}
	if got, err := ParseCPUStat(throttledCPUStat); err != nil || got != want {
		t.Fatalf("ParseCPUStat = %+v, %v, want %+v", got, err, want)
	}
}

func TestRunnerThrottleRead(t *testing.T) {
	t.Parallel()
	var argv []string
	if _, err := ReadRunnerThrottle(context.Background(), func(_ context.Context, args ...string) (string, error) {
		argv = args
		return throttledCPUStat, nil
	}); err != nil {
		t.Fatalf("ReadRunnerThrottle: %v", err)
	}
	want := "-n forgejo exec deploy/forgejo-runner -c dind -- cat /sys/fs/cgroup/cpu.stat"
	if got := strings.Join(argv, " "); got != want {
		t.Fatalf("ReadRunnerThrottle ran %q, want %q", got, want)
	}
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
		{"an empty answer", "", true, func(context.Context, ...string) (string, error) { return "", nil }, "carries no nr_periods"},
		{"an exec error", "", true, func(context.Context, ...string) (string, error) { return "", errors.New("kubectl is unreachable") }, "kubectl is unreachable"},
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
// sends: ReadRunnerThrottle passes a copy of the package's argv, not the slice.
func TestReadRunnerThrottleArgvIsCopied(t *testing.T) {
	t.Parallel()
	want := "-n forgejo exec deploy/forgejo-runner -c dind -- cat /sys/fs/cgroup/cpu.stat"
	var sent []string
	exec := func(_ context.Context, args ...string) (string, error) {
		sent = append(sent, strings.Join(args, " "))
		args[0] = "-n corrupted"
		return throttledCPUStat, nil
	}
	for range 2 {
		if _, err := ReadRunnerThrottle(context.Background(), exec); err != nil {
			t.Fatalf("ReadRunnerThrottle: %v", err)
		}
	}
	for i, got := range sent {
		if got != want {
			t.Errorf("read %d sent %q, want %q", i+1, got, want)
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
