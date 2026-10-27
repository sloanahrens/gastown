package land

import "testing"

// The kind a review is attributed to comes from the author's branch alone, so
// a review keeps the kind it was judged under whatever the bead's assignee
// says later (gt-b9j3o.16).
func TestAuthorKindTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, branch, want string
	}{
		{"a polecat branch", "polecat/corpus/gt-1+mv1", AuthorKindPolecat},
		{"a polecat seat named overseer is still a polecat's", "polecat/overseer/gt-1+mv1", AuthorKindPolecat},
		{"a crew branch", "crew/sloan/gt-1", AuthorKindCrew},
		{"the overseer's own crew branch", "crew/overseer/gt-1", AuthorKindOverseer},
		{"a branch prefix the town does not use", "sloan/post-land-shell-scope", AuthorKindUnknown},
		{"no branch", "", AuthorKindUnknown},
		{"a branch that names no seat", "polecat/corpus", AuthorKindUnknown},
		{"a crew branch that names no seat", "crew/sloan", AuthorKindUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AuthorKind(tc.branch); got != tc.want {
				t.Errorf("AuthorKind(%q) = %q, want %q", tc.branch, got, tc.want)
			}
		})
	}
}

// The seat is named exactly when the kind is: a branch whose seat cannot be
// read is unknown, never a seat guessed from the prefix.
func TestAuthorSeatIsEmptyExactlyWhenAuthorKindIsUnknown(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"polecat/corpus/gt-1+mv1", "crew/sloan/gt-1", "crew/overseer/gt-1", "sloan/gt-1", "", "polecat/"} {
		seat := AuthorSeat(branch)
		if (seat == "") != (AuthorKind(branch) == AuthorKindUnknown) {
			t.Errorf("AuthorSeat(%q) = %q and AuthorKind = %q: the seat and the kind disagree about whether a seat is named",
				branch, seat, AuthorKind(branch))
		}
	}
}
