package metrics

import (
	"math"
	"testing"
)

// wilsonTol is the tolerance the worked values are compared at: the panel's
// interval is drawn to a tenth of a percent, not to the last bit.
const wilsonTol = 0.001

// TestWilson is the 95% Wilson interval on worked counts, at both ends of the
// range where the normal approximation collapses, and with no trials to
// estimate from.
func TestWilson(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		k, n      int
		low, high float64
		ok        bool
	}{
		{"middle of the range", 15, 36, 0.271, 0.578, true},
		{"a thin tail", 5, 39, 0.056, 0.267, true},
		{"no positives", 0, 39, 0, 0.090, true},
		{"all positives", 39, 39, 0.910, 1, true},
		{"no trials", 0, 0, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			low, high, ok := wilsonInterval(tc.k, tc.n)
			if ok != tc.ok {
				t.Fatalf("wilsonInterval(%d, %d) ok = %v, want %v", tc.k, tc.n, ok, tc.ok)
			}
			if !ok {
				return
			}
			if math.Abs(low-tc.low) > wilsonTol || math.Abs(high-tc.high) > wilsonTol {
				t.Errorf("wilsonInterval(%d, %d) = %.4f..%.4f, want %.3f..%.3f", tc.k, tc.n, low, high, tc.low, tc.high)
			}
			if low < 0 || high > 1 || low > high {
				t.Errorf("wilsonInterval(%d, %d) = %.6f..%.6f, want a bound inside 0..1 in order", tc.k, tc.n, low, high)
			}
		})
	}
}
