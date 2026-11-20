package daemon

import (
	"testing"
	"time"
)

// A fresh id is allowed; an id that failed 29 minutes ago is not and one that
// failed 30 minutes ago is; three failures bar the id for good; the empty id is
// never allowed.
func TestNodePruneBackoffDecides(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		id       string
		failures int
		at       time.Time
		want     bool
	}{
		{"fresh", "run-1", 0, now, true},
		{"29 minutes", "run-2", 1, now.Add(-29 * time.Minute), false},
		{"30 minutes", "run-3", 1, now.Add(-30 * time.Minute), true},
		{"three failures", "run-4", 3, now.Add(-24 * time.Hour), false},
		{"empty id", "", 1, now.Add(-24 * time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b nodePruneBackoff
			for range tt.failures {
				b.Failed(tt.id, tt.at)
			}
			if got := b.Allowed(tt.id, now); got != tt.want {
				t.Errorf("Allowed(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
