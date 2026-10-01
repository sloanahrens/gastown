package exectax

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// timed answers a probe with a scripted exec, one entry per sample, and
// records every path it was handed.
type timed struct {
	times []time.Duration
	err   error
	paths []string
	// seen records, per sample, whether the file existed and was executable
	// when the exec was handed to the fake.
	seen []bool
}

func (t *timed) exec(_ context.Context, path string) (time.Duration, error) {
	t.paths = append(t.paths, path)
	info, err := os.Stat(path)
	t.seen = append(t.seen, err == nil && info.Mode().Perm()&0o100 != 0)
	i := len(t.paths) - 1
	if t.err != nil {
		return 0, t.err
	}
	if i >= len(t.times) {
		return 0, errors.New("more samples than scripted times")
	}
	return t.times[i], nil
}

func TestProbeReturnsTheMedianOfEverySample(t *testing.T) {
	t.Parallel()
	f := &timed{times: []time.Duration{9 * time.Millisecond, 3 * time.Millisecond, 7 * time.Millisecond, 5 * time.Millisecond, 200 * time.Millisecond}}
	res, err := Probe(context.Background(), Options{Samples: 5, Exec: f.exec})
	if err != nil {
		t.Fatal(err)
	}
	if res.Median != 7*time.Millisecond {
		t.Errorf("Median = %s, want 7ms (the middle sample, not the mean)", res.Median)
	}
	if len(res.Samples) != 5 || res.Samples[4] != 200*time.Millisecond {
		t.Errorf("Samples = %v, want the five the exec was handed, in order", res.Samples)
	}
}

func TestProbeWritesTheSamplesIntoItsOwnDirectory(t *testing.T) {
	t.Parallel()
	f := &timed{times: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	if _, err := Probe(context.Background(), Options{Samples: 3, Exec: f.exec}); err != nil {
		t.Fatal(err)
	}
	if len(f.paths) != 3 {
		t.Fatalf("exec'd %d programs, want one per sample", len(f.paths))
	}
	// The tax is on a NEW file: a probe that exec'd one file three times
	// would measure a warm exec and report the tax as absent.
	seen := map[string]bool{}
	for i, p := range f.paths {
		if seen[p] {
			t.Errorf("sample %d exec'd %s twice", i, p)
		}
		seen[p] = true
		if !f.seen[i] {
			t.Errorf("sample %d: %s was not an executable file at exec time", i, p)
		}
	}
	if dir := filepath.Dir(f.paths[0]); dir == "." || dir == "/" {
		t.Errorf("sample dir = %q, want a temp dir the probe made", dir)
	}
	for _, p := range f.paths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s outlived the probe: a probe must leave nothing behind", p)
		}
	}
}

func TestProbeStopsOnAFailedExec(t *testing.T) {
	t.Parallel()
	f := &timed{err: errors.New("exec format error")}
	_, err := Probe(context.Background(), Options{Samples: 2, Exec: f.exec})
	if err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("Probe = %v, want the exec's own error", err)
	}
	if len(f.paths) != 1 {
		t.Errorf("exec'd %d programs after the first failed, want 1", len(f.paths))
	}
}

func TestProbeStopsOnACancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &timed{times: []time.Duration{time.Millisecond}}
	if _, err := Probe(ctx, Options{Samples: 2, Exec: f.exec}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe = %v, want context.Canceled", err)
	}
	if len(f.paths) != 0 {
		t.Errorf("exec'd %d programs after the context was cancelled, want 0", len(f.paths))
	}
}

func TestTaxedJudgesTheMedianAgainstTheThreshold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		median time.Duration
		red    time.Duration
		want   bool
	}{
		{"under", 8 * time.Millisecond, DefaultRed, false},
		{"at the threshold", DefaultRed, DefaultRed, true},
		{"over", 180 * time.Millisecond, DefaultRed, true},
		{"a threshold of zero is off", time.Minute, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (Result{Median: tt.median}).Taxed(tt.red); got != tt.want {
				t.Errorf("Taxed(%s) at median %s = %v, want %v", tt.red, tt.median, got, tt.want)
			}
		})
	}
}

func TestStateOfReadsTheReportsMilliseconds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ms   *float64
		want State
	}{
		{"no measurement", nil, State{}},
		{"a negative count is not a measurement", ptr(-1.0), State{}},
		{"clear", ptr(8.0), State{Known: true, Median: 8 * time.Millisecond}},
		{"taxed", ptr(180.0), State{Known: true, Taxed: true, Median: 180 * time.Millisecond}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := StateOf(tt.ms, DefaultRed); got != tt.want {
				t.Errorf("StateOf(%v) = %+v, want %+v", tt.ms, got, tt.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestTransitionLogsOnlyAChange(t *testing.T) {
	t.Parallel()
	clear := State{Known: true, Median: 8 * time.Millisecond}
	taxed := State{Known: true, Taxed: true, Median: 180 * time.Millisecond}
	tests := []struct {
		name       string
		prev, now  State
		wantLog    bool
		mustHoldIn []string
	}{
		{name: "no baseline", prev: State{}, now: taxed},
		{name: "still clear", prev: clear, now: clear},
		{name: "still taxed", prev: taxed, now: taxed},
		{name: "no measurement now", prev: taxed, now: State{}},
		{name: "clear to taxed", prev: clear, now: taxed, wantLog: true, mustHoldIn: []string{"180ms", "gt-2ycne.1"}},
		{name: "taxed to clear", prev: taxed, now: clear, wantLog: true, mustHoldIn: []string{"clear", "8ms"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Transition(tt.prev, tt.now)
			if (got != "") != tt.wantLog {
				t.Fatalf("Transition(%+v, %+v) = %q, want logged=%v", tt.prev, tt.now, got, tt.wantLog)
			}
			for _, want := range tt.mustHoldIn {
				if !strings.Contains(got, want) {
					t.Errorf("Transition line %q does not name %q", got, want)
				}
			}
		})
	}
}
