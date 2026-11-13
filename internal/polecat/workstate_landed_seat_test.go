package polecat

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/land/landtest"
)

// TestLandedSeatDoesNotCountTowardCapacityAtTheManager drives the Manager's own
// classifier input (workstateInputForPolecat), the production caller that reads
// the seat's intent record and landing log. A submitted seat whose landing is
// recorded frees its capacity; a submission with no record still holds it.
func TestLandedSeatDoesNotCountTowardCapacityAtTheManager(t *testing.T) {
	t.Parallel()
	submittedAt := time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC)
	landedAfter := submittedAt.Add(time.Hour)
	cases := []struct {
		name     string
		landedAt *time.Time // the landing record's time; nil means none
		want     bool       // CountsTowardCapacity
	}{
		{name: "a landing recorded after the submission frees the seat", landedAt: &landedAfter, want: false},
		{name: "a submission with no landing record holds the seat", want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")
			if mgr.townRoot == "" {
				t.Fatal("manager has no town root; the landing read would not reach the town's records")
			}
			bd.Seed(beads.Issue{ID: "gt-jhz3t0", Status: "hooked", Labels: []string{land.LabelReadyToLand}})
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) { f.HookBead = "gt-jhz3t0" })
			seat := intent.Seat{Rig: "rig", Role: constants.RolePolecat, Name: "toast"}
			if err := intent.MarkSubmitted(mgr.townRoot, seat, "gt-jhz3t0", "test", submittedAt); err != nil {
				t.Fatal(err)
			}
			if c.landedAt != nil {
				landtest.WriteLandingLog(t, mgr.townRoot, "rig", land.LandingRecord{
					BeadID: "gt-jhz3t0", Rig: "rig", Head: "abc123", LandedAt: *c.landedAt,
				})
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateSubmitted, "")
			if d.CountsTowardCapacity != c.want {
				t.Fatalf("CountsTowardCapacity = %v, want %v (verdict %q, reason %q)",
					d.CountsTowardCapacity, c.want, d.Verdict, d.Reason)
			}
		})
	}
}
