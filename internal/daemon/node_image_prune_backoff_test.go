package daemon

import (
	"testing"
	"time"
)

// A fresh id is allowed; an id whose last failure was 29 minutes ago is not and
// one whose last failure was 30 minutes ago is; three failures bar the id for
// good; the empty id is never allowed. The window runs from the last of two
// failures, and two failures are still under the limit.
func TestNodePruneBackoffDecides(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		id       string
		failures []time.Time
		want     bool
	}{
		{"fresh", "run-1", nil, true},
		{"29 minutes", "run-2", []time.Time{now.Add(-29 * time.Minute)}, false},
		{"30 minutes", "run-3", []time.Time{now.Add(-30 * time.Minute)}, true},
		{"three failures", "run-4", []time.Time{now.Add(-24 * time.Hour), now.Add(-24 * time.Hour), now.Add(-24 * time.Hour)}, false},
		{"two failures, 29 minutes after the last", "run-5", []time.Time{now.Add(-59 * time.Minute), now.Add(-29 * time.Minute)}, false},
		{"two failures, 30 minutes after the last", "run-6", []time.Time{now.Add(-60 * time.Minute), now.Add(-30 * time.Minute)}, true},
		{"empty id", "", []time.Time{now.Add(-24 * time.Hour)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b nodePruneBackoff
			for _, at := range tt.failures {
				b.Failed(tt.id, at)
			}
			if got := b.Allowed(tt.id, now); got != tt.want {
				t.Errorf("Allowed(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
