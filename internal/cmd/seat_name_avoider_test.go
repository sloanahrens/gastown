package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// TestSeatNameAvoider judges the avoider a sling sets on the name pool. A seat's
// sandbox is named session.PolecatSessionName, so the guard is keyed by that
// name, not by the bare seat name.
func TestSeatNameAvoider(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	sandboxName := func(seat string) string { return session.PolecatSessionName("gt", seat) }

	staleDag := daemon.NewSeatNameGuard([]sandbox.Entry{{Name: "gt-dag", Phase: "Error"}}, now)
	readyNux := daemon.NewSeatNameGuard([]sandbox.Entry{{Name: "gt-nux", Phase: "Ready"}}, now)
	otherRigDag := daemon.NewSeatNameGuard([]sandbox.Entry{{Name: "bd-dag", Phase: "Error"}}, now)

	tests := []struct {
		name  string
		guard *daemon.SeatNameGuard
		seat  string
		want  bool
	}{
		{"stale sandbox avoids its seat", staleDag, "dag", true},
		{"stale sandbox leaves another seat free", staleDag, "nux", false},
		{"ready sandbox avoids nothing", readyNux, "nux", false},
		{"stale sandbox of another prefix avoids nothing", otherRigDag, "dag", false},
		{"nil guard avoids dag", nil, "dag", false},
		{"nil guard avoids nux", nil, "nux", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			avoid := seatNameAvoider(tc.guard, sandboxName)
			if got := avoid(tc.seat); got != tc.want {
				t.Errorf("avoid(%q) = %v, want %v", tc.seat, got, tc.want)
			}
		})
	}
}

// TestSeatNameAvoiderAvoidsEverySeatWithoutASandboxName: with a guard but no
// way to name a seat's sandbox, the avoider cannot rule a stale sandbox out, so
// it refuses every seat rather than drawing one that may be stale.
func TestSeatNameAvoiderAvoidsEverySeatWithoutASandboxName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	guard := daemon.NewSeatNameGuard([]sandbox.Entry{{Name: "gt-dag", Phase: "Error"}}, now)
	avoid := seatNameAvoider(guard, nil)
	for _, seat := range []string{"dag", "nux"} {
		if !avoid(seat) {
			t.Errorf("avoid(%q) = false, want true when no sandbox name is mapped", seat)
		}
	}
}
