package specdispatch

import (
	"strings"
	"testing"
	"time"
)

const claimTestFile = "src/lib/__tests__/wanderHops.test.ts"

// claimTestNow is the tick clock the rule is called with. The rule reads no
// time since gt-dnp9c removed the landing window, so the rows do not vary it.
var claimTestNow = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)

func TestIsTestFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"src/lib/__tests__/wanderHops.test.ts", true},
		{"wanderHops.test.ts", true},
		{"wanderHops.spec.tsx", true},
		{"internal/specdispatch/claim_test.go", true},
		{"tests/test_claim.py", true},
		{"src/lib/tour.ts", false},
		{"internal/specdispatch/claim.go", false},
		{"docs/guide.md", false},
		{"wanderHops.test", false},
	} {
		if got := IsTestFile(tc.path); got != tc.want {
			t.Errorf("IsTestFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestTargetClaimHold is the rule gt-n6jbw added and gt-dnp9c narrowed: a test
// file another bead claims by the same full path is one target, and the state
// of the claiming bead decides whether the claim holds.
func TestTargetClaimHold(t *testing.T) {
	t.Parallel()
	filed := Claimant{
		ID: "fr-zdzs", Status: "open", Priority: 1,
		CreatedAt: "2026-10-31T15:00:00Z",
		Paths:     []string{claimTestFile},
	}
	slatedFrZ64c := map[string]bool{"fr-z64c": true}
	slatedNobody := map[string]bool{"fr-other": true}

	tests := []struct {
		name      string
		peer      Claimant
		candidate Claimant
		slated    map[string]bool
		want      bool
		wantTxt   string
	}{
		{
			name:    "a bead in flight on the file holds it",
			peer:    Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: "fractals/polecats/cantor", Paths: []string{claimTestFile}},
			want:    true,
			wantTxt: "fr-z64c names the same test file, wanderHops.test.ts, and is in flight on it (fractals/polecats/cantor)",
		},
		{
			name:    "an open peer the tick slates ahead of it holds it",
			peer:    Claimant{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T09:00:00Z", Paths: []string{claimTestFile}},
			want:    true,
			wantTxt: "comes first in dispatch order",
		},
		{
			name: "an open peer behind in dispatch order does not hold it",
			peer: Claimant{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T20:00:00Z", Paths: []string{claimTestFile}},
		},
		{
			name: "an open peer the tick does not slate does not hold it",
			// A board-carried bead the tick holds for a reason of its own — the
			// shape gate refuses it, it needs planning — is never slung, so
			// ranking it first would hold the file for as long as its own hold
			// lasts.
			peer:   Claimant{ID: "fr-z64c", Status: "open", Priority: 0, CreatedAt: "2026-10-31T09:00:00Z", Paths: []string{claimTestFile}},
			slated: slatedNobody,
		},
		{
			name: "two test files of one name in different packages do not hold each other",
			// Packages repeat test-file names; only the whole path says which
			// file a bead means.
			peer:      Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: "fractals/polecats/cantor", Paths: []string{"internal/specdispatch/claim_test.go"}},
			candidate: Claimant{ID: "fr-zdzs", Paths: []string{"internal/cmd/claim_test.go"}},
		},
		{
			name: "a closed bead that did not land does not hold it",
			peer: Claimant{ID: "fr-z64c", Status: "closed", ClosedAt: "2026-10-31T17:00:00Z", Paths: []string{claimTestFile}},
		},
		{
			name: "two different tests in one file do not hold each other",
			peer: Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: "fractals/polecats/cantor",
				Paths: []string{claimTestFile}, Tests: []string{"TestShare"}},
			candidate: Claimant{ID: "fr-zdzs", Paths: []string{claimTestFile}, Tests: []string{"TestReach"}},
		},
		{
			name: "one shared test in one file holds it",
			peer: Claimant{ID: "fr-z64c", Status: "hooked", Assignee: "fractals/polecats/cantor",
				Paths: []string{claimTestFile}, Tests: []string{"TestShare"}},
			candidate: Claimant{ID: "fr-zdzs", Paths: []string{claimTestFile}, Tests: []string{"TestShare", "TestReach"}},
			want:      true,
			wantTxt:   "fr-z64c",
		},
		{
			name: "a shared plain file is not a target claim",
			peer: Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: "fractals/polecats/cantor",
				Paths: []string{"src/lib/tour.ts"}},
		},
		{
			name: "a peer naming no file claims nothing",
			peer: Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: "fractals/polecats/cantor"},
		},
		{
			name: "the candidate does not hold itself",
			peer: Claimant{ID: "fr-zdzs", Status: "in_progress", Assignee: "fractals/polecats/cantor", Paths: []string{claimTestFile}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := tc.candidate
			if c.ID == "" {
				c = filed
			}
			slated := tc.slated
			if slated == nil {
				slated = slatedFrZ64c
			}
			why, held := TargetClaimHold(c, []Claimant{tc.peer}, slated, claimTestNow)
			if held != tc.want {
				t.Fatalf("held = %v (%q), want %v", held, why, tc.want)
			}
			if tc.wantTxt != "" && !strings.Contains(why, tc.wantTxt) {
				t.Errorf("reason %q should name %q", why, tc.wantTxt)
			}
			if !tc.want && why != "" {
				t.Errorf("unheld candidate came back with a reason: %q", why)
			}
		})
	}
}

// TestClaimLandedPeerDoesNotHold is gt-dnp9c's first acceptance row: a peer
// that landed after the candidate was filed holds nothing, because a polecat
// starts from main and already has that landing.
func TestClaimLandedPeerDoesNotHold(t *testing.T) {
	t.Parallel()
	candidate := Claimant{
		ID: "fr-zdzs", Status: "open", Priority: 1,
		CreatedAt: "2026-10-31T12:00:00Z",
		Paths:     []string{claimTestFile},
	}
	for _, tc := range []struct {
		name string
		peer Claimant
	}{
		{
			name: "a landing after the candidate was filed",
			peer: Claimant{ID: "fr-z64c", Status: "closed", Landed: true, ClosedAt: "2026-10-31T17:00:00Z", Paths: []string{claimTestFile}},
		},
		{
			name: "a landing before the candidate was filed",
			peer: Claimant{ID: "fr-z64c", Status: "closed", Landed: true, ClosedAt: "2026-10-31T09:00:00Z", Paths: []string{claimTestFile}},
		},
		{
			name: "a landing whose times cannot be read",
			peer: Claimant{ID: "fr-z64c", Status: "closed", Landed: true, ClosedAt: "not a time", Paths: []string{claimTestFile}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if why, held := TargetClaimHold(candidate, []Claimant{tc.peer}, map[string]bool{}, claimTestNow); held {
				t.Fatalf("held = true (%q), want a landed peer to hold nothing", why)
			}
		})
	}
}

// TestClaimBareNameAndUnreadClaimDoNotHold is gt-dnp9c's second acceptance row:
// a peer that names the file only by a bare name, or whose claim could not be
// read, names no full path the rule can compare, so it holds nothing.
func TestClaimBareNameAndUnreadClaimDoNotHold(t *testing.T) {
	t.Parallel()
	const bareName = "wanderHops.test.ts"
	const seat = "fractals/polecats/cantor"
	fullPath := Claimant{ID: "fr-zdzs", Status: "open", Priority: 1, CreatedAt: "2026-10-31T15:00:00Z", Paths: []string{claimTestFile}}
	bare := Claimant{ID: "fr-zdzs", Status: "open", Priority: 1, CreatedAt: "2026-10-31T15:00:00Z", Paths: []string{bareName}}

	for _, tc := range []struct {
		name      string
		candidate Claimant
		peer      Claimant
		slated    map[string]bool
	}{
		{
			name:      "a peer that spells the file bare",
			candidate: fullPath,
			peer:      Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: seat, Paths: []string{bareName}},
		},
		{
			name:      "a candidate that spells the file bare",
			candidate: bare,
			peer:      Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: seat, Paths: []string{claimTestFile}},
		},
		{
			name:      "both spelling the file bare",
			candidate: bare,
			peer:      Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: seat, Paths: []string{bareName}},
		},
		{
			name:      "a peer whose claim was not read",
			candidate: fullPath,
			peer:      Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: seat, ClaimUnread: true},
		},
		{
			// The list row can still carry the path while design and notes go
			// unread. The rule must read the flag, not the leftover path: an
			// unread claim is unknown, so it holds nothing (gt-dnp9c).
			name:      "an unread peer whose row still spells the file",
			candidate: fullPath,
			peer:      Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: seat, ClaimUnread: true, Paths: []string{claimTestFile}},
		},
		{
			name:      "an unread peer slated ahead",
			candidate: fullPath,
			peer:      Claimant{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T09:00:00Z", ClaimUnread: true},
			slated:    map[string]bool{"fr-z64c": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if why, held := TargetClaimHold(tc.candidate, []Claimant{tc.peer}, tc.slated, claimTestNow); held {
				t.Fatalf("held = true (%q), want no hold", why)
			}
		})
	}
}

// TestUnreadClaimPeers names the peers whose unknown claim must be reported:
// one in flight, or one the tick slates ahead, holds nothing in
// TargetClaimHold, so the tick says which peer it could not read rather than
// letting the unknown claim look idle (gt-dnp9c).
func TestUnreadClaimPeers(t *testing.T) {
	t.Parallel()
	const seat = "fractals/polecats/cantor"
	candidate := Claimant{
		ID: "fr-zdzs", Status: "open", Priority: 1,
		CreatedAt: "2026-10-31T15:00:00Z",
		Paths:     []string{claimTestFile},
	}
	for _, tc := range []struct {
		name   string
		peers  []Claimant
		slated map[string]bool
		want   []string
	}{
		{
			name:  "an unread peer in flight is named",
			peers: []Claimant{{ID: "fr-z64c", Status: "in_progress", Assignee: seat, ClaimUnread: true}},
			want:  []string{"fr-z64c"},
		},
		{
			name:  "an unread hooked peer is named",
			peers: []Claimant{{ID: "fr-z64c", Status: "hooked", Assignee: seat, ClaimUnread: true}},
			want:  []string{"fr-z64c"},
		},
		{
			name:   "an unread peer slated ahead is named",
			peers:  []Claimant{{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T09:00:00Z", ClaimUnread: true}},
			slated: map[string]bool{"fr-z64c": true},
			want:   []string{"fr-z64c"},
		},
		{
			name:   "an unread peer the tick does not slate is not named",
			peers:  []Claimant{{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T09:00:00Z", ClaimUnread: true}},
			slated: map[string]bool{"fr-other": true},
		},
		{
			name:   "an unread peer behind in dispatch order is not named",
			peers:  []Claimant{{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T20:00:00Z", ClaimUnread: true}},
			slated: map[string]bool{"fr-z64c": true},
		},
		{
			name:  "a closed unread peer is not named",
			peers: []Claimant{{ID: "fr-z64c", Status: "closed", ClaimUnread: true}},
		},
		{
			name:  "a read peer is not named even in flight on the path",
			peers: []Claimant{{ID: "fr-z64c", Status: "in_progress", Assignee: seat, Paths: []string{claimTestFile}}},
		},
		{
			name:  "the candidate's own unread claim is not named",
			peers: []Claimant{{ID: candidate.ID, Status: "in_progress", Assignee: seat, ClaimUnread: true}},
		},
		{
			name:   "the names are sorted",
			peers:  []Claimant{{ID: "fr-zzz", Status: "in_progress", Assignee: seat, ClaimUnread: true}, {ID: "fr-aaa", Status: "in_progress", Assignee: seat, ClaimUnread: true}},
			want:   []string{"fr-aaa", "fr-zzz"},
			slated: map[string]bool{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := UnreadClaimPeers(candidate, tc.peers, tc.slated)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("UnreadClaimPeers() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClaimInFlightExactPathStillHolds is gt-dnp9c's third acceptance row: the
// in-flight and slated-ahead rules survive for two beads that spell the same
// full path, and the reason names the peer.
func TestClaimInFlightExactPathStillHolds(t *testing.T) {
	t.Parallel()
	candidate := Claimant{
		ID: "fr-zdzs", Status: "open", Priority: 1,
		CreatedAt: "2026-10-31T15:00:00Z",
		Paths:     []string{claimTestFile},
	}
	for _, tc := range []struct {
		name   string
		peer   Claimant
		slated map[string]bool
	}{
		{
			name: "a peer in flight on the path",
			peer: Claimant{ID: "fr-z64c", Status: "in_progress", Assignee: "fractals/polecats/cantor", Paths: []string{claimTestFile}},
		},
		{
			name: "a hooked peer on the path",
			peer: Claimant{ID: "fr-z64c", Status: "hooked", Assignee: "fractals/polecats/cantor", Paths: []string{claimTestFile}},
		},
		{
			name:   "a slated peer ahead in dispatch order",
			peer:   Claimant{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T09:00:00Z", Paths: []string{claimTestFile}},
			slated: map[string]bool{"fr-z64c": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			why, held := TargetClaimHold(candidate, []Claimant{tc.peer}, tc.slated, claimTestNow)
			if !held {
				t.Fatalf("held = false, want a hold on the same full path")
			}
			if !strings.Contains(why, tc.peer.ID) {
				t.Errorf("reason %q should name %q", why, tc.peer.ID)
			}
		})
	}
}

// TestClaimHandoffBeadIsNotAPeer is gt-d5f5q's first acceptance row: a handoff
// note carries an in-flight status for as long as its session runs and cites
// whatever that session read, so it is no peer a candidate waits behind.
func TestClaimHandoffBeadIsNotAPeer(t *testing.T) {
	t.Parallel()
	candidate := Claimant{
		ID: "gt-new", Status: "open", Priority: 2,
		CreatedAt: "2026-11-02T12:00:00Z",
		Paths:     []string{"internal/bdshim/differential_helpers_test.go"},
	}
	for _, tc := range []struct {
		name     string
		labels   []string
		assignee string
	}{
		{name: "the crew spelling", labels: []string{"handoff"}, assignee: "town/crew/dipper"},
		{name: "the runtime spelling", labels: []string{"gt:handoff"}, assignee: "town/crew/dipper"},
		{name: "a handoff note among other labels", labels: []string{"gt:task", "handoff"}, assignee: "town/crew/dipper"},
		// The label alone marks the note: a polecat's own handoff bead is
		// still not work on the file (gt-asvow).
		{name: "a polecat's handoff note", labels: []string{"handoff"}, assignee: "gastown/polecats/dipper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			peer := Claimant{
				ID: "gt-dipper", Status: "in_progress", Assignee: tc.assignee,
				Labels: tc.labels,
				Paths:  []string{"internal/bdshim/differential_helpers_test.go"},
			}
			if why, held := TargetClaimHold(candidate, []Claimant{peer}, map[string]bool{}, claimTestNow); held {
				t.Fatalf("held = true (%q), want a handoff note to hold nothing", why)
			}
		})
	}
}

// TestClaimCrewAssigneeIsNotAPeer is gt-d5f5q's second acceptance row: a bead
// held by a crew or bird identity is not work on the file, while the same bead
// on a polecat seat still holds the candidate and the reason names the seat.
func TestClaimCrewAssigneeIsNotAPeer(t *testing.T) {
	t.Parallel()
	const path = "internal/bdshim/differential_helpers_test.go"
	candidate := Claimant{ID: "gt-new", Status: "open", Priority: 2, Paths: []string{path}}
	for _, tc := range []struct {
		name     string
		assignee string
		want     bool
	}{
		{name: "a crew session's bead does not hold", assignee: "town/crew/overseer"},
		{name: "a bird's bead does not hold", assignee: "gastown/witness"},
		{name: "a polecat seat's bead still holds", assignee: "gastown/polecats/bullet", want: true},
		{name: "another rig's polecat seat still holds", assignee: "fractals/polecats/cantor", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			peer := Claimant{ID: "gt-peer", Status: "in_progress", Assignee: tc.assignee, Paths: []string{path}}
			why, held := TargetClaimHold(candidate, []Claimant{peer}, map[string]bool{}, claimTestNow)
			if held != tc.want {
				t.Fatalf("held = %v (%q), want %v", held, why, tc.want)
			}
			if tc.want && !strings.Contains(why, tc.assignee) {
				t.Errorf("reason %q should name the seat %q", why, tc.assignee)
			}
		})
	}
}

// TestTargetClaimHoldLowestIDAnswers keeps a bead several peers claim on one
// reason, so its skip line does not change with the pool's order.
func TestTargetClaimHoldLowestIDAnswers(t *testing.T) {
	t.Parallel()
	candidate := Claimant{ID: "fr-zdzs", Paths: []string{claimTestFile}}
	peers := []Claimant{
		{ID: "fr-z64c", Status: "in_progress", Assignee: "fractals/polecats/cantor", Paths: []string{claimTestFile}},
		{ID: "fr-aaa", Status: "open", Paths: []string{claimTestFile}},
	}
	why, held := TargetClaimHold(candidate, peers, map[string]bool{"fr-aaa": true}, claimTestNow)
	if !held {
		t.Fatal("expected a hold")
	}
	if !strings.HasPrefix(why, "fr-aaa ") {
		t.Errorf("reason %q should name the lowest claiming id", why)
	}
}

func TestCloseReasonIsLanding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason string
		want   bool
	}{
		{"Landed on main\ntarget_branch: main\ncommit_sha: 5be95dd\nlanded_commit: 5be95dd\npatch_id: 8e16", true},
		{"Landed on main", true},
		{"no-changes: superseded before landing by fr-zdzs", false},
		{"closed after review", false},
		{"", false},
	} {
		if got := CloseReasonIsLanding(tc.reason); got != tc.want {
			t.Errorf("CloseReasonIsLanding(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}
