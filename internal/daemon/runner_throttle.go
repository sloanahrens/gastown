package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// RunnerThrottle is one cumulative reading of the runner container's cgroup v2 cpu.stat.
type RunnerThrottle struct {
	Periods       int64
	Throttled     int64
	ThrottledUsec int64
}

// runnerThrottleKeys are the counters a reading needs, in missing-key order.
var runnerThrottleKeys = []string{"nr_periods", "nr_throttled", "throttled_usec"}

// runnerThrottleArgs reads the forgejo-runner container's cpu.stat through kubectl.
var runnerThrottleArgs = []string{"-n", "forgejo", "exec", "deploy/forgejo-runner", "-c", "dind", "--", "cat", "/sys/fs/cgroup/cpu.stat"}

// ParseCPUStat reads cgroup v2 cpu.stat's three throttle counters. A missing
// counter or a line that is not one count errors naming the key, never zero.
func ParseCPUStat(text string) (RunnerThrottle, error) {
	counts := make(map[string]int64, len(runnerThrottleKeys))
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !slices.Contains(runnerThrottleKeys, key) {
			continue
		}
		if _, dup := counts[key]; dup {
			return RunnerThrottle{}, fmt.Errorf("cpu.stat names %s twice", key)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			return RunnerThrottle{}, fmt.Errorf("cpu.stat's %s is not a count", key)
		}
		counts[key] = n
	}
	for _, key := range runnerThrottleKeys {
		if _, ok := counts[key]; !ok {
			return RunnerThrottle{}, fmt.Errorf("cpu.stat carries no %s", key)
		}
	}
	return RunnerThrottle{Periods: counts["nr_periods"], Throttled: counts["nr_throttled"], ThrottledUsec: counts["throttled_usec"]}, nil
}

// ReadRunnerThrottle reads the runner container's cpu.stat through exec, the
// caller's seam; a missing seam or a failed read is an error, never a zero.
func ReadRunnerThrottle(ctx context.Context, exec func(ctx context.Context, args ...string) (string, error)) (RunnerThrottle, error) {
	if exec == nil {
		return RunnerThrottle{}, errors.New("no exec seam, so the runner's cpu.stat cannot be read")
	}
	text, err := exec(ctx, runnerThrottleArgs...)
	if err == nil {
		var t RunnerThrottle
		if t, err = ParseCPUStat(text); err == nil {
			return t, nil
		}
	}
	return RunnerThrottle{}, fmt.Errorf("reading the runner's cpu.stat: %w", err)
}
