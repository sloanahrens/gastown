package main

import (
	"errors"
	"testing"
	"time"
)

// millis returns durations of the given milliseconds, in the order given.
func millis(ms ...int) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, v := range ms {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

// hundredDescending is 100, 99, ... 1 ms: a fixed list whose nearest-rank
// percentiles are their own numbers, and which is out of order for Summarize.
func hundredDescending() []time.Duration {
	out := make([]time.Duration, 0, 100)
	for v := 100; v >= 1; v-- {
		out = append(out, time.Duration(v)*time.Millisecond)
	}
	return out
}

func TestPercentiles(t *testing.T) {
	t.Parallel()

	t.Run("nearest rank on a sorted list", func(t *testing.T) {
		t.Parallel()
		sorted := make([]time.Duration, 0, 100)
		for v := 1; v <= 100; v++ {
			sorted = append(sorted, time.Duration(v)*time.Millisecond)
		}
		for _, tc := range []struct {
			percent int
			want    time.Duration
		}{
			{percent: 0, want: 1 * time.Millisecond},
			{percent: 1, want: 1 * time.Millisecond},
			{percent: 50, want: 50 * time.Millisecond},
			{percent: 95, want: 95 * time.Millisecond},
			{percent: 99, want: 99 * time.Millisecond},
			{percent: 100, want: 100 * time.Millisecond},
		} {
			if got := Percentile(sorted, tc.percent); got != tc.want {
				t.Errorf("Percentile(p%d) = %s, want %s", tc.percent, got, tc.want)
			}
		}
	})

	t.Run("rank rounds up on a short list", func(t *testing.T) {
		t.Parallel()
		sorted := millis(10, 20, 30)
		if got, want := Percentile(sorted, 50), 20*time.Millisecond; got != want {
			t.Errorf("p50 of [10 20 30] = %s, want %s", got, want)
		}
		if got, want := Percentile(sorted, 95), 30*time.Millisecond; got != want {
			t.Errorf("p95 of [10 20 30] = %s, want %s", got, want)
		}
	})

	t.Run("summarize sorts a copy and reads the percentiles", func(t *testing.T) {
		t.Parallel()
		in := hundredDescending()
		got, err := Summarize(in)
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		want := Summary{P50: 50 * time.Millisecond, P95: 95 * time.Millisecond, P99: 99 * time.Millisecond, Max: 100 * time.Millisecond}
		if got != want {
			t.Errorf("Summarize = %+v, want %+v", got, want)
		}
		if in[0] != 100*time.Millisecond {
			t.Errorf("Summarize reordered its input: first is %s", in[0])
		}
	})

	t.Run("summarize refuses an empty run", func(t *testing.T) {
		t.Parallel()
		if _, err := Summarize(nil); !errors.Is(err, ErrNoSamples) {
			t.Errorf("Summarize(nil) error = %v, want ErrNoSamples", err)
		}
	})

	t.Run("verdict at the design budget", func(t *testing.T) {
		t.Parallel()
		base := Baseline{P50: 40 * time.Millisecond, P95: 40 * time.Millisecond}
		for _, tc := range []struct {
			name string
			s    Summary
			want bool
		}{
			{
				name: "every overhead exactly on its limit passes",
				s:    Summary{P50: 55 * time.Millisecond, P95: 80 * time.Millisecond, Max: 240 * time.Millisecond},
				want: true,
			},
			{
				name: "median overhead one nanosecond over fails",
				s:    Summary{P50: 55*time.Millisecond + 1, P95: 80 * time.Millisecond, Max: 100 * time.Millisecond},
				want: false,
			},
			{
				name: "p95 overhead one nanosecond over fails",
				s:    Summary{P50: 40 * time.Millisecond, P95: 80*time.Millisecond + 1, Max: 100 * time.Millisecond},
				want: false,
			},
			{
				name: "one call 200 ms past the median plus one nanosecond fails",
				s:    Summary{P50: 40 * time.Millisecond, P95: 40 * time.Millisecond, Max: 240*time.Millisecond + 1},
				want: false,
			},
			{
				name: "a gateway faster than the baseline passes",
				s:    Summary{P50: 10 * time.Millisecond, P95: 20 * time.Millisecond, Max: 30 * time.Millisecond},
				want: true,
			},
		} {
			if got := Verdict(tc.s, base); got != tc.want {
				t.Errorf("%s: Verdict = %t, want %t", tc.name, got, tc.want)
			}
		}
	})
}
