package cmd

// poolOccupancyTally counts seats: Total every seat that holds a pool seat, and
// each flag the seats carrying it.
type poolOccupancyTally struct {
	Total      int
	Claim      int
	DeadHooked int
	// DeadHookedByClass splits DeadHooked by each seat's deadClass (gt-fp32o8).
	DeadHookedByClass map[string]int
	Landing           int
	Sessions          int
}

// poolOccupancyCount is the pool's one count of the seats it holds, in total
// and per agent.
type poolOccupancyCount struct {
	poolOccupancyTally
	ByAgent map[string]poolOccupancyTally
}

// add counts one seat that holds a pool seat.
func (t *poolOccupancyTally) add(s poolSession) {
	t.Total++
	if s.claim {
		t.Claim++
	}
	if s.deadHooked {
		t.DeadHooked++
		if t.DeadHookedByClass == nil {
			t.DeadHookedByClass = map[string]int{}
		}
		t.DeadHookedByClass[s.deadClass]++
	}
	if s.landing {
		t.Landing++
	}
	if !s.claim && !s.deadHooked && !s.landing {
		t.Sessions++
	}
}

// poolOccupancy counts the seats in sessions. Every seat counts toward a seat
// cap except a parkedStale one, whose seat is released (gt-t7k92), so a
// parkedStale seat counts in no field. The flags are not exclusive, and nothing
// is relabelled:
//
//   - Claim: a seat another sling reserved before its session existed (claim).
//   - DeadHooked: a seat whose polecat crashed with its bead still hooked (deadHooked).
//   - DeadHookedByClass: the DeadHooked seats split by their disposition class (deadClass), not a flag of its own.
//   - Landing: a seat mid-landing, whose polecat ran gt done and whose bead the landing worker has not closed (landing).
//   - Sessions: a seat carrying none of Claim, DeadHooked or Landing.
//
// A seat that is both claimed and dead-hooked counts once in Total, once in
// Claim and once in DeadHooked, and not in Sessions.
//
// The one deliberate exception is the patrol's restart census, livePolecatCensus
// in internal/daemon/patrol_scan_seatcap.go, which counts live sessions only. A
// restart replaces a seat its crashed polecat already holds, so counting its
// dead-hooked, landing and claim seats would hold that seat against itself
// (gt-zwiww).
func poolOccupancy(sessions []poolSession) poolOccupancyCount {
	c := poolOccupancyCount{ByAgent: map[string]poolOccupancyTally{}}
	for _, s := range sessions {
		if s.parkedStale {
			continue
		}
		c.poolOccupancyTally.add(s)
		t := c.ByAgent[s.agent]
		t.add(s)
		c.ByAgent[s.agent] = t
	}
	return c
}
