package metrics

import (
	"math"
	"strings"
	"testing"
)

// TestLongAUC: the figure is the share of (long, other) pairs est orders
// correctly, a tie counting one half.
func TestLongAUC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		est      []float64
		measured []float64
		minutes  float64
		want     float64
	}{
		{
			name:     "perfect ordering",
			est:      []float64{9, 8, 2, 1},
			measured: []float64{40, 25, 10, 3},
			minutes:  20,
			want:     1,
		},
		{
			name:     "reversed ordering",
			est:      []float64{1, 2, 8, 9},
			measured: []float64{40, 25, 10, 3},
			minutes:  20,
			want:     0,
		},
		{
			name:     "all equal scores",
			est:      []float64{5, 5, 5, 5},
			measured: []float64{40, 25, 10, 3},
			minutes:  20,
			want:     0.5,
		},
		{
			// Long {3, 2} against other {2, 1}: 3>2, 3>1, 2==2 at half, 2>1
			// gives 3.5 of 4 pairs.
			name:     "one tie in four pairs",
			est:      []float64{3, 2, 2, 1},
			measured: []float64{30, 25, 5, 4},
			minutes:  20,
			want:     0.875,
		},
		{
			// A bead measured at exactly the threshold is not over it.
			name:     "a bead at the threshold is not long",
			est:      []float64{9, 1},
			measured: []float64{21, 20},
			minutes:  20,
			want:     1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := LongAUC(tt.est, tt.measured, tt.minutes)
			if err != nil {
				t.Fatalf("LongAUC: %v", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("LongAUC = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLongAUCError: a series the figure cannot score is an error naming the
// cause, never a number over whatever pairs survived.
func TestLongAUCError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		est      []float64
		measured []float64
		minutes  float64
		want     string
	}{
		{
			name:     "unequal lengths",
			est:      []float64{1, 2},
			measured: []float64{30},
			minutes:  20,
			want:     "2 estimates, 1 measured",
		},
		{
			name:     "no bead over the threshold",
			est:      []float64{1, 2},
			measured: []float64{20, 10},
			minutes:  20,
			want:     "no bead ran over",
		},
		{
			name:     "every bead over the threshold",
			est:      []float64{1, 2},
			measured: []float64{30, 40},
			minutes:  20,
			want:     "every bead ran over",
		},
		{
			name:     "NaN estimate",
			est:      []float64{math.NaN(), 1},
			measured: []float64{30, 10},
			minutes:  20,
			want:     "estimate 0 is not finite",
		},
		{
			name:     "infinite measured minutes",
			est:      []float64{1, 2},
			measured: []float64{math.Inf(1), 10},
			minutes:  20,
			want:     "measured minutes 0 is not finite",
		},
		{
			name:     "non-finite threshold",
			est:      []float64{1, 2},
			measured: []float64{30, 10},
			minutes:  math.NaN(),
			want:     "long threshold",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LongAUC(tt.est, tt.measured, tt.minutes)
			if err == nil {
				t.Fatalf("LongAUC accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("LongAUC error = %q, want it to name %q", err, tt.want)
			}
		})
	}
}
