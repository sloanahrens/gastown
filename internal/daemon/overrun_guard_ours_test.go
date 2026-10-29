package daemon

import "testing"

// A bead the stopped seat still holds is stored with the seat's full assignee,
// so the ownership check must compare against that, not the bare polecat name.
func TestOverrunSeatAssignee(t *testing.T) {
	t.Parallel()
	seat := overrunSeat{Rig: "beads", Polecat: "abacus"}
	if got, want := overrunSeatAssignee(seat), "beads/polecats/abacus"; got != want {
		t.Errorf("overrunSeatAssignee() = %q, want %q", got, want)
	}
}

func TestOverrunSeatOwnsBead(t *testing.T) {
	t.Parallel()
	seat := overrunSeat{Rig: "beads", Polecat: "abacus"}
	cases := []struct {
		name     string
		status   string
		assignee string
		want     bool
	}{
		{name: "assigned to the seat", status: "hooked", assignee: "beads/polecats/abacus", want: true},
		{name: "unassigned", status: "hooked", assignee: "", want: true},
		{name: "bare polecat name", status: "hooked", assignee: "abacus", want: false},
		{name: "another polecat", status: "hooked", assignee: "beads/polecats/other", want: false},
		{name: "closed", status: "closed", assignee: "beads/polecats/abacus", want: false},
		{name: "tombstone", status: "tombstone", assignee: "beads/polecats/abacus", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := overrunSeatOwnsBead(seat, tc.status, tc.assignee); got != tc.want {
				t.Errorf("overrunSeatOwnsBead(%q, %q) = %v, want %v", tc.status, tc.assignee, got, tc.want)
			}
		})
	}
}
