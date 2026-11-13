package storypoints

import "testing"

// TestEstimateOf pins which bead labels carry one estimate. A bead with a
// single sp:N label has that estimate; a bead with none, with a label ParseLabel
// refuses, or with two different estimates has none (gt-cdhagm).
func TestEstimateOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		labels     []string
		wantPoints int
		wantFound  bool
	}{
		{"one estimate", []string{"gt:ready-to-land", "sp:8"}, 8, true},
		{"no labels", nil, 0, false},
		{"no estimate", []string{"sp-actual:5", "sp-rung:small"}, 0, false},
		{"estimate off the scale is refused", []string{"sp:4"}, 0, false},
		{"malformed estimate is refused", []string{"sp:05"}, 0, false},
		{"two different estimates conflict", []string{"sp:3", "sp:5"}, 0, false},
		{"a repeated estimate is one estimate", []string{"sp:3", "sp:3"}, 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotPoints, gotFound := EstimateOf(tt.labels)
			if gotPoints != tt.wantPoints || gotFound != tt.wantFound {
				t.Fatalf("EstimateOf(%q) = (%d, %v), want (%d, %v)",
					tt.labels, gotPoints, gotFound, tt.wantPoints, tt.wantFound)
			}
		})
	}
}
