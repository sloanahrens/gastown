package authorkind

import "testing"

// The kind a review is attributed to comes from the author's branch alone, so
// a review keeps the kind it was judged under whatever the bead's assignee
// says later (gt-b9j3o.14, gt-b9j3o.16).
func TestKindTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, branch, want string
	}{
		{"a polecat branch", "polecat/corpus/gt-1+mv1", Polecat},
		{"a polecat seat named overseer is still a polecat's", "polecat/overseer/gt-1+mv1", Polecat},
		{"a crew branch", "crew/sloan/gt-1", Crew},
		{"the overseer's own crew branch", "crew/overseer/gt-1", Overseer},
		{"a branch prefix the town does not use", "sloan/post-land-shell-scope", Unknown},
		{"no branch", "", Unknown},
		{"a branch that names no seat", "polecat/corpus", Unknown},
		{"a crew branch that names no seat", "crew/sloan", Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Kind(tc.branch); got != tc.want {
				t.Errorf("Kind(%q) = %q, want %q", tc.branch, got, tc.want)
			}
		})
	}
}

// The seat is named exactly when the kind is: a branch whose seat cannot be
// read is unknown, never a seat guessed from the prefix.
func TestSeatIsEmptyExactlyWhenKindIsUnknown(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"polecat/corpus/gt-1+mv1", "crew/sloan/gt-1", "crew/overseer/gt-1", "sloan/gt-1", "", "polecat/"} {
		seat := Seat(branch)
		if (seat == "") != (Kind(branch) == Unknown) {
			t.Errorf("Seat(%q) = %q and Kind = %q: the seat and the kind disagree about whether a seat is named",
				branch, seat, Kind(branch))
		}
	}
}
