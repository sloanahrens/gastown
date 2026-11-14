package main

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// The design's latency budget (docs/design/bd-container.md, "Latency budget"),
// read as overhead over the in-pod bd call: at most 15 ms at the median, 40 ms
// at the 95th percentile, and a hard failure when a single call runs 200 ms
// past the in-pod median.
const (
	budgetMedian = 15 * time.Millisecond
	budgetP95    = 40 * time.Millisecond
	budgetHard   = 200 * time.Millisecond
)

// showCalls and listCalls are the workload the design names: 200 show calls and
// 50 list --json calls, one call at a time.
const (
	showCalls = 200
	listCalls = 50
)

// ErrNoSamples is Summarize's refusal of an empty latency list: a run that made
// no calls has no percentiles, and reporting zeros would read as a clean run.
var ErrNoSamples = errors.New("bdgw-bench: no latency samples")

// Summary holds the percentiles the bench prints.
type Summary struct {
	P50, P95, P99, Max time.Duration
}

// Baseline is the in-pod bd latency the gateway's overhead is measured from.
type Baseline struct {
	P50, P95 time.Duration
}

// Percentile returns the nearest-rank percent-th percentile of sorted, which must
// be ascending and non-empty: the value at rank ceil(percent*n/100), clamped to
// the list. Integer arithmetic keeps a boundary rank exact.
func Percentile(sorted []time.Duration, percent int) time.Duration {
	rank := (percent*len(sorted) + 99) / 100
	rank = max(rank, 1)
	rank = min(rank, len(sorted))
	return sorted[rank-1]
}

// Summarize sorts a copy of latencies and returns its percentiles and maximum.
func Summarize(latencies []time.Duration) (Summary, error) {
	if len(latencies) == 0 {
		return Summary{}, ErrNoSamples
	}
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	return Summary{
		P50: Percentile(sorted, 50),
		P95: Percentile(sorted, 95),
		P99: Percentile(sorted, 99),
		Max: sorted[len(sorted)-1],
	}, nil
}

// Verdict reports whether the gateway stays inside the budget: the median and
// p95 overheads over the in-pod baseline within their limits, and no single call
// more than budgetHard over the in-pod median.
func Verdict(s Summary, base Baseline) bool {
	return s.P50-base.P50 <= budgetMedian &&
		s.P95-base.P95 <= budgetP95 &&
		s.Max-base.P50 <= budgetHard
}

// call sends one bd call's arguments to the gateway and returns an error when the
// gateway refused it or bd did not exit 0.
type call func(argv []string) error

// workload is the call list the design names: showCalls show calls of bead,
// then listCalls list --json calls.
func workload(bead string) [][]string {
	argvs := make([][]string, 0, showCalls+listCalls)
	for i := 0; i < showCalls; i++ {
		argvs = append(argvs, []string{"show", bead, "--json"})
	}
	for i := 0; i < listCalls; i++ {
		argvs = append(argvs, []string{"list", "--json"})
	}
	return argvs
}

// measure makes each call in order, one at a time, and returns each call's
// latency as now reads it. The first failed call ends the run: a failed call is
// not a latency sample, and a run with one is not a clean run.
func measure(argvs [][]string, do call, now func() time.Time) ([]time.Duration, error) {
	latencies := make([]time.Duration, 0, len(argvs))
	for i, argv := range argvs {
		start := now()
		if err := do(argv); err != nil {
			return nil, fmt.Errorf("call %d (%s): %w", i+1, argv[0], err)
		}
		latencies = append(latencies, now().Sub(start))
	}
	return latencies, nil
}
