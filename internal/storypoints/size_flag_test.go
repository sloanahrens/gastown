package storypoints

import (
	"math"
	"testing"
)

// TestSizeFlagTable pins the over-300-lines flag: the estimate threshold on the
// rungs that carry the prediction, and the inputs that can never flag.
func TestSizeFlagTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		minutes float64
		rung    string
		want    bool
	}{
		{"below the threshold", 9.9, "all", false},
		{"at the threshold", 10.0, "all", true},
		{"above the threshold", 10.8, "all", true},
		{"blended without embeddings", 10.0, "shape-jev", true},
		{"blended without Jev", 10.0, "shape-embed", true},
		{"below the threshold on shape", 9.9, "shape", false},
		{"at the threshold on shape", 10.0, "shape", false},
		{"above the threshold on shape", 10.8, "shape", false},
		{"NaN", math.NaN(), "all", false},
		{"positive infinity", math.Inf(1), "all", false},
		{"negative infinity", math.Inf(-1), "all", false},
		{"zero", 0, "all", false},
		{"negative", -1, "all", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SizeFlag(tt.minutes, tt.rung); got != tt.want {
				t.Errorf("SizeFlag(%v, %q) = %v, want %v", tt.minutes, tt.rung, got, tt.want)
			}
		})
	}
}
