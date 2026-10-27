package metrics

import "testing"

func TestAuthorKindTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"a polecat branch", "polecat/corpus/gt-1+mv1", AuthorKindPolecat},
		{"a polecat seat named overseer is still a polecat's", "polecat/overseer/gt-1+mv1", AuthorKindPolecat},
		{"a crew branch", "crew/sloan/gt-1", AuthorKindCrew},
		{"the overseer's own crew branch", "crew/overseer/gt-1", AuthorKindOverseer},
		{"a branch prefix the town does not use", "sloan/post-land-shell-scope", AuthorKindUnknown},
		{"no branch", "", AuthorKindUnknown},
		{"a branch that names no seat", "polecat/corpus", AuthorKindUnknown},
		{"a crew branch that names no seat", "crew/sloan", AuthorKindUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AuthorKind(tc.branch); got != tc.want {
				t.Errorf("AuthorKind(%q) = %q, want %q", tc.branch, got, tc.want)
			}
		})
	}
}

func TestAuthorSeatTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"a polecat branch", "polecat/corpus/gt-1+mv1", "corpus"},
		{"a crew branch", "crew/sloan/gt-1", "sloan"},
		{"a crew branch the overseer wrote", "crew/overseer/gt-1", "overseer"},
		{"a branch with no third segment", "polecat/corpus", ""},
		{"a branch prefix the town does not use", "sloan/post-land-shell-scope", ""},
		{"no branch", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AuthorSeat(tc.branch); got != tc.want {
				t.Errorf("AuthorSeat(%q) = %q, want %q", tc.branch, got, tc.want)
			}
		})
	}
}

// The two functions read one branch, so a branch a seat can be read from is
// never an unknown author, and a branch with no seat is never a seat kind: a
// crew/overseer branch is the overseer's work from either function.
func TestAuthorKindNamesASeatExactlyWhenAuthorSeatDoes(t *testing.T) {
	t.Parallel()
	branches := []string{
		"polecat/corpus/gt-1+mv1",
		"crew/sloan/gt-1",
		"crew/overseer/gt-1",
		"polecat/corpus",
		"crew/sloan",
		"sloan/post-land-shell-scope",
		"",
	}
	for _, branch := range branches {
		seat := AuthorSeat(branch)
		kind := AuthorKind(branch)
		if (seat == "") != (kind == AuthorKindUnknown) {
			t.Errorf("branch %q: seat %q, kind %q; want no seat exactly when the kind is unknown", branch, seat, kind)
		}
		if seat == "overseer" && kind != AuthorKindOverseer {
			t.Errorf("branch %q: seat %q, kind %q; want the overseer's kind", branch, seat, kind)
		}
	}
}

func TestBranchLineTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		notes string
		want  string
	}{
		{"one Branch line", "READY TO LAND\nBranch: polecat/opal/gt-1+mv1\nHead: abc\n", "polecat/opal/gt-1+mv1"},
		{
			name:  "the last Branch line is the live branch",
			notes: "READY TO LAND\nBranch: polecat/opal/gt-1+mv1\nHead: abc\n\nMERGE REJECTION (attempt 1): review - r\nBranch: polecat/opal/gt-1+mv2\nTarget: main\n",
			want:  "polecat/opal/gt-1+mv2",
		},
		{
			name:  "a quoted gate tail cannot name a branch",
			notes: "MERGE REJECTION (attempt 1): gate - red\nBranch: crew/sloan/gt-2\nGate tail:\n  | Branch: evil/branch\n",
			want:  "crew/sloan/gt-2",
		},
		{"a Branch line inside prose is not a line", "see the Branch: polecat/opal/gt-3 note\n", ""},
		{"a note with no Branch line", "READY TO LAND\nHead: abc\n", ""},
		{"an empty Branch line names nothing", "Branch: \n", ""},
		{"a trailing space and a CRLF", "Branch: crew/sloan/gt-4 \r\n", "crew/sloan/gt-4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BranchLine(tc.notes); got != tc.want {
				t.Errorf("BranchLine(%q) = %q, want %q", tc.notes, got, tc.want)
			}
		})
	}
}

// authorKindBdFixture holds one bead per attribution case: a polecat branch, a
// crew branch, a crew branch the overseer took over after the review, a bead
// with no Branch line, a bead whose Branch line is only in its description, and
// the overseer's own branch. Every bead carries the assignee bd sends, which
// attribution reads past: gt-5's branch is the crew's even though the bead is
// the overseer's now.
const authorKindBdFixture = `[
 {"id": "gt-1", "notes": "READY TO LAND\nBranch: polecat/opal/gt-1+mv1\nHead: abc\n", "assignee": "gastown/polecats/opal"},
 {"id": "gt-2", "notes": "READY TO LAND\nBranch: crew/merlin/gt-2\nHead: abc\n", "assignee": "gastown/crew/merlin"},
 {"id": "gt-3", "notes": "READY TO LAND\nHead: abc\n", "assignee": "gastown/polecats/robin"},
 {"id": "gt-4", "description": "Branch: polecat/emerald/gt-4+mv1", "assignee": "gastown/polecats/emerald"},
 {"id": "gt-5", "notes": "READY TO LAND\nBranch: crew/merlin/gt-5\nHead: abc\n", "assignee": "town/crew/overseer"},
 {"id": "gt-6", "notes": "READY TO LAND\nBranch: crew/overseer/gt-6\nHead: abc\n", "assignee": "town/crew/overseer"}
]`

var authorKindReviewLines = []string{
	`{"bead": "gt-1", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:00:00Z"}`,
	`{"bead": "gt-2", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:05:00Z"}`,
	`{"bead": "gt-3", "rig": "gastown", "attempt": 1, "score": 0.4, "verdict": "request_changes", "at": "2026-10-26T22:10:00Z"}`,
	`{"bead": "gt-4", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:12:00Z"}`,
	`{"bead": "gt-5", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:13:00Z"}`,
	`{"bead": "gt-6", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:14:00Z"}`,
	`{"bead": "gt-9", "rig": "gastown", "attempt": 2, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:15:00Z"}`,
}

// The attribution runs through the production loaders: bd's listing supplies
// the Branch line, the review log supplies the attempt, and AttributeReviews
// joins them.
func TestAttributeReviewsJoinsEachReviewToItsBeadsBranch(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	beads := LoadBeads(fakeBd(authorKindBdFixture, true, &calls), []string{"/g"}, &Dropped{})
	if len(beads) != 6 {
		t.Fatalf("beads = %d, want 6", len(beads))
	}
	if beads[0].Branch != "polecat/opal/gt-1+mv1" {
		t.Errorf("gt-1 branch = %q, want the listing's", beads[0].Branch)
	}
	if beads[3].Branch != "" {
		t.Errorf("gt-4 branch = %q, want none: a Branch line in the description is not the bead's", beads[3].Branch)
	}
	reviews := LoadReviews(authorKindReviewLines, &Dropped{})
	if len(reviews) != 7 {
		t.Fatalf("reviews = %d, want 7", len(reviews))
	}
	got := AttributeReviews(reviews, beads)
	want := []struct {
		author, kind string
		round        int
	}{
		{"opal", AuthorKindPolecat, 1},
		{"merlin", AuthorKindCrew, 1},
		{"", AuthorKindUnknown, 1},          // no Branch line on the bead
		{"", AuthorKindUnknown, 1},          // a Branch line only in the description names nothing
		{"merlin", AuthorKindCrew, 1},       // a crew branch the overseer took over after the review
		{"overseer", AuthorKindOverseer, 1}, // the overseer's own branch
		{"", AuthorKindUnknown, 2},          // a bead the listing does not hold
	}
	if len(got) != len(want) {
		t.Fatalf("attributed reviews = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Author != w.author || got[i].AuthorKind != w.kind || got[i].Attempt != w.round {
			t.Errorf("review %d (%s) = author %q kind %q attempt %d; want %q, %q, %d",
				i, got[i].Bead, got[i].Author, got[i].AuthorKind, got[i].Attempt, w.author, w.kind, w.round)
		}
	}
	if reviews[0].AuthorKind != "" {
		t.Errorf("input review kind = %q, want the input left unattributed", reviews[0].AuthorKind)
	}
}

// The round a review is counted under is the attempt its log line carries, not
// a recount of how many reviews the bead has.
func TestReviewAttemptIsTheAttemptTheReviewLogCarries(t *testing.T) {
	t.Parallel()
	reviews := LoadReviews([]string{
		`{"bead": "gt-1", "rig": "gastown", "attempt": 3, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:00:00Z"}`,
	}, &Dropped{})
	if len(reviews) != 1 {
		t.Fatalf("reviews = %d, want 1", len(reviews))
	}
	if reviews[0].Attempt != 3 {
		t.Errorf("attempt = %d, want the log's 3", reviews[0].Attempt)
	}
}

func TestAttributeReviewsWithNoBeadsIsUnknownNotPolecat(t *testing.T) {
	t.Parallel()
	got := AttributeReviews([]Review{{Bead: "gt-1", Attempt: 1}}, nil)
	if got[0].AuthorKind != AuthorKindUnknown {
		t.Errorf("kind = %q, want %q", got[0].AuthorKind, AuthorKindUnknown)
	}
}
