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
