package cmd

import (
	"reflect"
	"testing"
)

// TestAdmissionCountsAClaimAndItsSeatOnce pins that begin counts a sling's
// claim and the occupied seat hooking the same bead as one seat (gt-uwq612), in
// the dry run and the live decision alike: counting both reads one polecat as
// two seats and refuses another sling a seat the town still has free.
func TestAdmissionCountsAClaimAndItsSeatOnce(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{false, true} {
		name := "dry run"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newTestPoolTown(t, cappedPool(2))
			const otherPID = 9001
			if _, err := w.process().seats.publish(poolSeatClaim{
				ID: "other-1", PID: otherPID, Agent: claimAgent, Bead: "gt-a", CreatedAt: poolTestNow,
			}); err != nil {
				t.Fatal(err)
			}
			// The seat the claim's sling has spawned already hooks the same bead.
			occupied := func() ([]poolSession, error) {
				return []poolSession{{name: "gt-ember", agent: claimAgent, hookedBead: "gt-a"}}, nil
			}

			sessions, d, err := w.process().seats.begin(live, occupied)
			if err != nil {
				t.Fatalf("begin(live=%v): %v", live, err)
			}
			d.done()
			got := make([]string, len(sessions))
			for i, s := range sessions {
				got[i] = s.name
			}
			if want := []string{"gt-ember"}; !reflect.DeepEqual(got, want) {
				t.Errorf("begin(live=%v) counted %v, want %v (the claim and the seat hooking gt-a are one)", live, got, want)
			}
		})
	}
}
