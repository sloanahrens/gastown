package bdgateway

import (
	"testing"
	"time"
)

// TestRefusalWindow checks every call's write and suppressed count across five steps.
func TestRefusalWindow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	var w refusalWindow
	tests := []struct {
		name           string
		now            time.Time
		calls          int
		wantWrite      bool
		wantSuppressed uint64
	}{
		{"first call writes", start, 1, true, 0},
		{"calls two to ten write", start, 9, true, 0},
		{"call eleven is refused and counted", start, 1, false, 0},
		{"next window's first call writes and reports the drop", start.Add(time.Minute), 1, true, 1},
		{"next window's second call reports nothing", start.Add(time.Minute + time.Second), 1, true, 0},
	}
	for _, tc := range tests {
		for i := 0; i < tc.calls; i++ {
			write, suppressed := w.allow(tc.now)
			if write != tc.wantWrite || suppressed != tc.wantSuppressed {
				t.Errorf("%s, call %d: allow = (%t, %d), want (%t, %d)", tc.name, i+1, write, suppressed, tc.wantWrite, tc.wantSuppressed)
			}
		}
	}
}
