package storypoints

import (
	"math"
	"testing"
)

// TestActual pins the points a landing's polecat time earns, and the weighted
// flag that says whether it earns any. A time that is absent, not positive, not
// finite, or a bound is unweighted and carries no points.
func TestActual(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		secs       float64
		lowerBound bool
		upperBound bool
		wantPoints int
		wantWeight bool
	}{
		{"ten minutes is eight", 600, false, false, 8, true},
		{"sixty minutes is fifty-five", 3600, false, false, 55, true},
		{"one second is one, weighted", 1, false, false, 1, true},
		{"zero seconds is unweighted", 0, false, false, 0, false},
		{"negative seconds is unweighted", -60, false, false, 0, false},
		{"NaN is unweighted", math.NaN(), false, false, 0, false},
		{"positive infinity is unweighted", math.Inf(1), false, false, 0, false},
		{"negative infinity is unweighted", math.Inf(-1), false, false, 0, false},
		{"lower-bound time is unweighted", 600, true, false, 0, false},
		{"upper-bound time is unweighted", 600, false, true, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotPoints, gotWeighted := Actual(tt.secs, tt.lowerBound, tt.upperBound)
			if gotPoints != tt.wantPoints || gotWeighted != tt.wantWeight {
				t.Fatalf("Actual(%v, %v, %v) = (%d, %v), want (%d, %v)",
					tt.secs, tt.lowerBound, tt.upperBound,
					gotPoints, gotWeighted, tt.wantPoints, tt.wantWeight)
			}
		})
	}
}
