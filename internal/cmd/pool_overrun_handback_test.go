package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// TestPoolRosterDoesNotCountAHandedBackSeat is the overrun guard's release
// (gt-stu4g) read by the roster the spec dispatcher ticks on. After the stop,
// polecat.HandBack leaves the bead open with no assignee, and the seat's agent
// bead names no hook (the slot is no longer maintained): the stopped seat holds
// nothing, so it is not dead-hooked and the pool has its seat back. Before the
// hand-back the same seat, its bead still assigned, filled the pool.
func TestPoolRosterDoesNotCountAHandedBackSeat(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 1}}}
	ts := config.NewTownSettings()
	ts.PolecatPool = pool

	stuck := &beads.AgentFields{AgentState: "working"}

	cases := []struct {
		name     string
		assigned *beads.Issue
		wantDead int
	}{
		{"before the hand-back the stopped seat is dead-hooked", &beads.Issue{ID: "gt-92qx3", Status: "hooked", Assignee: "gastown/polecats/ember"}, 1},
		{"after the hand-back the bead is nobody's and the seat is free", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "ember")
			reads := poolSeatReadsAnswering(t, stuck, assessPolecatAssignedIssueWork(c.assigned))

			sessions, err := poolSeatSessionsWith(&fakeLister{}, town, poolDispositionAnswering(town, reads), nil, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			r := specRosterFrom(sessions, ts)
			if r.DeadHooked["deepseek-flash"] != c.wantDead || r.Live["deepseek-flash"] != c.wantDead {
				t.Fatalf("roster = %d live, %d dead-hooked, want %d of each (%+v)",
					r.Live["deepseek-flash"], r.DeadHooked["deepseek-flash"], c.wantDead, sessions)
			}
			_, reason, refused, _ := choosePoolAgent(pool, "", sessions)
			if refused != (c.wantDead > 0) {
				t.Fatalf("refused = %v, want %v (%s)", refused, c.wantDead > 0, reason)
			}
		})
	}
}
