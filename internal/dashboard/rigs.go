package dashboard

// The Rigs panel: one row per known rig, with the numbers that say where work
// is sitting. It is a join of two readers the hub already polls — the work
// queue (each store's ready beads and landing queue) and the polecat seats —
// so it reads nothing of its own. The rig list is the rig registry's.
//
// A rig that is parked is stood down on purpose, not broken: it keeps its row
// and its numbers, marked parked.

// Rig is one row of the Rigs panel. A count the dashboard could not read is a
// nil pointer, which the page renders "unavailable" — never as zero, which is
// a reading of its own.
type Rig struct {
	// Name is the rig's name in the registry.
	Name string `json:"name"`
	// Parked marks a rig an operator stood down. The dispatcher does not serve
	// a parked rig, so its work does not move until it is re-enabled.
	Parked bool `json:"parked,omitempty"`
	// Seats is how many of the rig's polecats hold nothing and are free to take
	// work. Nil when the seat reading itself failed.
	Seats *int `json:"seats,omitempty"`
	// Ready and Landing are the rig's own store's beads: open with nothing
	// blocking them, and submitted and waiting to land. Both nil for a store
	// that could not be read.
	Ready   *int `json:"ready,omitempty"`
	Landing *int `json:"landing,omitempty"`
}

// RigRows joins the queue's per-rig reading with the polecat seats. The queue
// rows already carry a store's park state and, for a store it read, its ready
// and landing counts; the seat count is the polecats' to know. A nil polecat
// list is a seat reading that failed, and leaves every Seats nil.
func RigRows(q *Queue, polecats []Polecat) []Rig {
	if q == nil || len(q.Rigs) == 0 {
		return nil
	}
	seats := map[string]int{}
	for _, p := range polecats {
		if p.State == StateIdle {
			seats[p.Rig]++
		}
	}
	out := make([]Rig, 0, len(q.Rigs))
	for _, r := range q.Rigs {
		if polecats != nil {
			n := seats[r.Name]
			r.Seats = &n
		}
		out = append(out, r)
	}
	return out
}
