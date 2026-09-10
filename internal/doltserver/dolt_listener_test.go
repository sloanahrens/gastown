package doltserver

import "testing"

// gt-vzxq: a listener's PPID is only diagnostic once it's already been
// excluded from the expected-port set (production is deliberately started
// via a short-lived CLI invocation, so its PPID becomes 1 by design). These
// tests cover IsOrphaned in isolation.
func TestDoltListenerIsOrphaned(t *testing.T) {
	tests := []struct {
		name string
		ppid int
		want bool
	}{
		{"reparented to init is orphaned", 1, true},
		{"live parent is not orphaned", 4242, false},
		{"unknown ppid (0) is not treated as orphaned", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := DoltListener{PID: 100, Port: 53236, PPID: tt.ppid}
			if got := l.IsOrphaned(); got != tt.want {
				t.Errorf("IsOrphaned() with PPID=%d = %v, want %v", tt.ppid, got, tt.want)
			}
		})
	}
}
