package health

import (
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// gt-vzxq: the Dolt health check must distinguish the production server
// from correctly-isolated test servers. A count-based check that flags any
// non-expected-port Dolt listener as a "zombie" fires on hermetic test
// infrastructure behaving exactly as intended (ephemeral port, own temp
// data dir, live parent) — the worst direction for a false positive,
// because it trains agents to discount the alarm that would matter.
func TestClassifyListeners(t *testing.T) {
	tests := []struct {
		name          string
		listeners     []doltserver.DoltListener
		expectedPorts []int
		wantZombies   []int
		wantLiveTests []int
	}{
		{
			name:          "no listeners is healthy",
			listeners:     nil,
			expectedPorts: []int{3307},
		},
		{
			name: "production-only is healthy",
			listeners: []doltserver.DoltListener{
				{PID: 8009, Port: 3307, PPID: 1},
			},
			expectedPorts: []int{3307},
		},
		{
			name: "hermetic test server with live parent is not a zombie",
			listeners: []doltserver.DoltListener{
				{PID: 8009, Port: 3307, PPID: 1},
				{PID: 55617, Port: 53236, PPID: 4242},
			},
			expectedPorts: []int{3307},
			wantLiveTests: []int{55617},
		},
		{
			name: "orphaned test server (reparented to init) is a zombie",
			listeners: []doltserver.DoltListener{
				{PID: 8009, Port: 3307, PPID: 1},
				{PID: 55617, Port: 53236, PPID: 1},
			},
			expectedPorts: []int{3307},
			wantZombies:   []int{55617},
		},
		{
			name: "mixed: one orphan, one live test server",
			listeners: []doltserver.DoltListener{
				{PID: 8009, Port: 3307, PPID: 1},
				{PID: 111, Port: 40001, PPID: 1},
				{PID: 222, Port: 40002, PPID: 4242},
			},
			expectedPorts: []int{3307},
			wantZombies:   []int{111},
			wantLiveTests: []int{222},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyListeners(tt.listeners, tt.expectedPorts)

			if got.Count != len(tt.wantZombies) || !intSlicesEqual(got.PIDs, tt.wantZombies) {
				t.Errorf("zombies = %v (count %d), want %v", got.PIDs, got.Count, tt.wantZombies)
			}
			if got.LiveTestCount != len(tt.wantLiveTests) || !intSlicesEqual(got.LiveTestPIDs, tt.wantLiveTests) {
				t.Errorf("live test servers = %v (count %d), want %v", got.LiveTestPIDs, got.LiveTestCount, tt.wantLiveTests)
			}
		})
	}
}

func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
