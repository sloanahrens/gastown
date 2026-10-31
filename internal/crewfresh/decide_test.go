package crewfresh

import "testing"

// decideCase is one row of the decision table: the state git reported, and the
// report every caller must get for it.
type decideCase struct {
	name        string
	state       State
	wantAction  Action
	wantBehind  int
	wantMessage string
}

var decideCases = []decideCase{
	{
		name:        "clean main behind origin/main",
		state:       State{FetchOK: true, Branch: "main", Clean: true, Behind: 5, HeadIsAncestor: true},
		wantAction:  ActionFastForward,
		wantBehind:  5,
		wantMessage: "behind 5 and moved",
	},
	{
		name:        "up to date",
		state:       State{FetchOK: true, Branch: "main", Clean: true, Behind: 0, HeadIsAncestor: true},
		wantAction:  ActionNone,
		wantBehind:  0,
		wantMessage: "up to date",
	},
	{
		name:        "on another branch",
		state:       State{FetchOK: true, Branch: "crew/sloan/gt-9", Clean: true, Behind: 3, HeadIsAncestor: true},
		wantAction:  ActionNone,
		wantBehind:  3,
		wantMessage: "behind 3 but on branch crew/sloan/gt-9 so main is left alone",
	},
	{
		name:        "detached head",
		state:       State{FetchOK: true, Branch: "", Clean: true, Behind: 3, HeadIsAncestor: true},
		wantAction:  ActionNone,
		wantBehind:  3,
		wantMessage: "behind 3 but on no branch so main is left alone",
	},
	{
		name:        "local changes",
		state:       State{FetchOK: true, Branch: "main", Clean: false, Behind: 4, HeadIsAncestor: true},
		wantAction:  ActionNone,
		wantBehind:  4,
		wantMessage: "behind 4 but the clone has local changes so nothing moved",
	},
	{
		name:        "local commits on main",
		state:       State{FetchOK: true, Branch: "main", Clean: true, Behind: 2, HeadIsAncestor: false},
		wantAction:  ActionNone,
		wantBehind:  2,
		wantMessage: "behind 2 but main has local commits so nothing moved",
	},
	{
		name:        "fetch failed",
		state:       State{FetchOK: false, Branch: "main", Clean: true, Behind: 7, HeadIsAncestor: true},
		wantAction:  ActionNone,
		wantBehind:  7,
		wantMessage: "fetch failed so the numbers are as of the last fetch",
	},
	{
		name:        "on another branch and dirty",
		state:       State{FetchOK: true, Branch: "crew/sloan/gt-9", Clean: false, Behind: 6, HeadIsAncestor: true},
		wantAction:  ActionNone,
		wantBehind:  6,
		wantMessage: "behind 6 but on branch crew/sloan/gt-9 so main is left alone",
	},
	{
		name:        "changes unreadable",
		state:       State{FetchOK: true, Branch: "main", CleanUnreadable: true, Behind: 3, HeadIsAncestor: true},
		wantAction:  ActionNone,
		wantBehind:  3,
		wantMessage: "behind 3 but the clone's changes could not be read so nothing moved",
	},
	{
		name:        "ancestry unreadable",
		state:       State{FetchOK: true, Branch: "main", Clean: true, Behind: 3, AncestryUnreadable: true},
		wantAction:  ActionNone,
		wantBehind:  3,
		wantMessage: "behind 3 but main's ancestry could not be read so nothing moved",
	},
	{
		name:        "changes unreadable on another branch keeps the branch line",
		state:       State{FetchOK: true, Branch: "crew/sloan/gt-9", CleanUnreadable: true, Behind: 3, HeadIsAncestor: true},
		wantAction:  ActionNone,
		wantBehind:  3,
		wantMessage: "behind 3 but on branch crew/sloan/gt-9 so main is left alone",
	},
	{
		name:        "dirty clone keeps the changes line when ancestry is unreadable",
		state:       State{FetchOK: true, Branch: "main", Clean: false, Behind: 3, AncestryUnreadable: true},
		wantAction:  ActionNone,
		wantBehind:  3,
		wantMessage: "behind 3 but the clone has local changes so nothing moved",
	},
}

// A fast-forward is the only ref move this package allows, and it needs every
// condition at once: fetch ok, on main, clean, strict ancestor and behind
// (gt-g4p2j).
func TestDecideFastForwardsOnlyWhenCleanMainAncestorBehind(t *testing.T) {
	t.Parallel()
	for _, tc := range decideCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Decide(tc.state); got.Action != tc.wantAction {
				t.Errorf("Decide(%+v).Action = %d, want %d", tc.state, got.Action, tc.wantAction)
			}
		})
	}
}

// Every blocked case says what stopped the move, in one of the fixed lines,
// and the report carries the count the line was written from (gt-g4p2j).
func TestDecideMessagesNameWhatBlockedTheMove(t *testing.T) {
	t.Parallel()
	for _, tc := range decideCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Decide(tc.state)
			if got.Message != tc.wantMessage {
				t.Errorf("Decide(%+v).Message = %q, want %q", tc.state, got.Message, tc.wantMessage)
			}
			if got.Behind != tc.wantBehind {
				t.Errorf("Decide(%+v).Behind = %d, want %d", tc.state, got.Behind, tc.wantBehind)
			}
		})
	}
}
