package metrics

import (
	"math"
	"strings"
	"testing"
)

// TestLongAUC: the figure is the share of (long, other) pairs est orders
// correctly, a tie counting one half, and a series it cannot score is an error
// naming the cause.
func TestLongAUC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		est      []float64
		measured []float64
		minutes  float64
		want     float64
		wantErr  string
	}{
		{name: "perfect ordering", est: []float64{9, 8, 2, 1}, measured: []float64{40, 25, 10, 3}, minutes: 20, want: 1},
		{name: "reversed ordering", est: []float64{1, 2, 8, 9}, measured: []float64{40, 25, 10, 3}, minutes: 20, want: 0},
		{name: "all equal scores", est: []float64{5, 5, 5, 5}, measured: []float64{40, 25, 10, 3}, minutes: 20, want: 0.5},
		{name: "one tie in four pairs (3 of 4 wins plus half of 1)", est: []float64{3, 2, 2, 1}, measured: []float64{30, 25, 5, 4}, minutes: 20, want: 0.875},
		{name: "a bead at the threshold is not long", est: []float64{9, 1}, measured: []float64{21, 20}, minutes: 20, want: 1},
		{name: "unequal lengths", est: []float64{1, 2}, measured: []float64{30}, minutes: 20, wantErr: "2 estimates, 1 measured"},
		{name: "no bead over the threshold", est: []float64{1, 2}, measured: []float64{20, 10}, minutes: 20, wantErr: "no bead ran over 20 minutes"},
		{name: "every bead over the threshold", est: []float64{1, 2}, measured: []float64{30, 40}, minutes: 20, wantErr: "every bead ran over 20 minutes"},
		{name: "NaN estimate", est: []float64{math.NaN(), 1}, measured: []float64{30, 10}, minutes: 20, wantErr: "estimate 0 is not finite"},
		{name: "infinite measured minutes", est: []float64{1, 2}, measured: []float64{math.Inf(1), 10}, minutes: 20, wantErr: "measured minutes 0 is not finite"},
		{name: "non-finite threshold", est: []float64{1, 2}, measured: []float64{30, 10}, minutes: math.NaN(), wantErr: "long threshold NaN is not finite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := LongAUC(tt.est, tt.measured, tt.minutes)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LongAUC error = %v, want it to name %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LongAUC: %v", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("LongAUC = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLongAUCInterval: a perfect estimate gives the interval 1..1 (a one-class
// resample is skipped, not scored 0.5), one seed gives one interval, and a
// series it cannot resample is an error naming the cause.
func TestLongAUCInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		est       []float64
		measured  []float64
		wantErr   string
		resamples int
	}{
		{name: "perfect ordering skips the one-class resamples", est: []float64{9, 2, 1, 0.5}, measured: []float64{30, 10, 5, 3}},
		{name: "no resample drew both classes", est: []float64{1, 2}, measured: []float64{20, 10}, wantErr: "0 of 40 resamples usable"},
		{name: "unequal lengths", est: []float64{1, 2}, measured: []float64{30}, wantErr: "2 estimates, 1 measured"},
		{name: "a NaN estimate is refused", est: []float64{9, 2, math.NaN(), 0.5}, measured: []float64{30, 10, 5, 3}, wantErr: "not finite"},
		{name: "negative resamples", est: []float64{9, 2, 1, 0.5}, measured: []float64{30, 10, 5, 3}, resamples: -1, wantErr: "resamples"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := 40
			if tt.resamples != 0 {
				n = tt.resamples
			}
			lo, hi, err := LongAUCInterval(tt.est, tt.measured, 20, n, 1)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || lo != 0 || hi != 0 {
					t.Fatalf("LongAUCInterval = %v..%v, %v, want 0..0 and an error naming %q", lo, hi, err, tt.wantErr)
				}
				return
			}
			if err != nil || lo != 1 || hi != 1 {
				t.Fatalf("LongAUCInterval = %v..%v, %v, want 1..1", lo, hi, err)
			}
		})
	}
	est, measured := []float64{5, 4, 3, 2, 1, 1, 1}, []float64{40, 10, 30, 5, 25, 22, 21}
	lo, hi, err := LongAUCInterval(est, measured, 20, 40, 7)
	if err != nil {
		t.Fatalf("LongAUCInterval: %v", err)
	}
	point, perr := LongAUC(est, measured, 20)
	if perr != nil || lo < 0 || hi > 1 || lo > hi || point < lo || point > hi {
		t.Fatalf("interval %v..%v, want 0 <= lo <= hi <= 1 around the point %v (%v)", lo, hi, point, perr)
	}
	for range 3 {
		lo2, hi2, err2 := LongAUCInterval(est, measured, 20, 40, 7)
		if err2 != nil || lo2 != lo || hi2 != hi {
			t.Fatalf("LongAUCInterval with one seed = %v..%v then %v..%v, %v, want the same", lo, hi, lo2, hi2, err2)
		}
	}
}

// TestLongAUCIntervalRefuses: an interval needs two resamples, and at least half
// of them must draw both classes. A series whose resamples come out one-class
// more often than not is an error naming the usable count; a balanced series
// returns an interval. One long bead in 40 draws the long bead in about 64% of
// its resamples, so it stays above half; a one-long, one-short series is the
// shape that can fall below it.
func TestLongAUCIntervalRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		est       []float64
		measured  []float64
		resamples int
		seed      uint64
		wantErr   string
	}{
		{name: "one resample", est: []float64{9, 1, 2}, measured: []float64{40, 10, 5}, resamples: 1, seed: 1, wantErr: "1 resamples, want at least 2"},
		{name: "one long bead among two keeps under half usable", est: []float64{9, 1}, measured: []float64{40, 10}, resamples: 400, seed: 4, wantErr: "192 of 400 resamples usable"},
		{name: "balanced series", est: []float64{9, 1, 8, 2}, measured: []float64{40, 10, 30, 5}, resamples: 400, seed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lo, hi, err := LongAUCInterval(tt.est, tt.measured, 20, tt.resamples, tt.seed)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || lo != 0 || hi != 0 {
					t.Fatalf("LongAUCInterval = %v..%v, %v, want 0..0 and an error naming %q", lo, hi, err, tt.wantErr)
				}
				return
			}
			if err != nil || lo < 0 || hi > 1 || lo > hi {
				t.Fatalf("LongAUCInterval = %v..%v, %v, want an interval 0 <= lo <= hi <= 1", lo, hi, err)
			}
		})
	}
}
