package cmd

import (
	"reflect"
	"testing"
)

// TestPoolReconcile compares fake running sandboxes with fake counted seats.
// Each row names one state the comparison must answer.
func TestPoolReconcile(t *testing.T) {
	t.Parallel()
	live := poolSession{name: "gt-live", agent: "a"}
	deadHooked := poolSession{name: "gt-ember", agent: "a", deadHooked: true}
	claim := poolSession{name: "pool-claim/x", agent: "a", claim: true}
	landing := poolSession{name: "gt-landing", agent: "a", landing: true}
	parked := poolSession{name: "gt-parked", agent: "a", parkedStale: true}
	tests := []struct {
		name          string
		counted       []poolSession
		running       []string
		wantUncounted []string
		wantNoSandbox []string
	}{
		{
			name:    "counted seat with its sandbox is in neither list",
			counted: []poolSession{live},
			running: []string{"gt-live"},
		},
		{
			name:          "running sandbox with no counted seat is uncounted",
			counted:       []poolSession{live},
			running:       []string{"gt-live", "gt-stray"},
			wantUncounted: []string{"gt-stray"},
		},
		{
			name:          "counted dead-hooked seat with no sandbox is in NoSandbox",
			counted:       []poolSession{deadHooked},
			wantNoSandbox: []string{"gt-ember"},
		},
		{
			name:    "claim and landing seats with no sandbox are not listed",
			counted: []poolSession{claim, landing},
		},
		{
			name:    "released parked seat with no sandbox is not listed",
			counted: []poolSession{parked},
		},
		{
			name:          "sandbox of a released parked seat is uncounted",
			counted:       []poolSession{parked},
			running:       []string{"gt-parked"},
			wantUncounted: []string{"gt-parked"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := poolReconcile(tc.counted, tc.running)
			if !reflect.DeepEqual(got.Uncounted, tc.wantUncounted) {
				t.Errorf("Uncounted = %v, want %v", got.Uncounted, tc.wantUncounted)
			}
			if !reflect.DeepEqual(got.NoSandbox, tc.wantNoSandbox) {
				t.Errorf("NoSandbox = %v, want %v", got.NoSandbox, tc.wantNoSandbox)
			}
		})
	}
}
