package specdispatch

import (
	"strings"
	"testing"
	"time"
)

const claimTestFile = "src/lib/__tests__/wanderHops.test.ts"

// claimTestNow is the tick clock the landing-window rows run at.
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

// TestTargetClaimHold is the rule gt-n6jbw added: a test file another bead
// claims is one target, whatever spelling of the path the two use, and the
// state of the claiming bead decides whether the claim holds.
func TestTargetClaimHold(t *testing.T) {
	t.Parallel()
	filed := Claimant{
		ID: "fr-zdzs", Status: "open", Priority: 1,
		CreatedAt: "2026-10-31T15:00:00Z",
		Paths:     []string{claimTestFile},
	}
	boardHoldsFrZ64c := map[string]bool{"fr-z64c": true}
	boardHoldsNobody := map[string]bool{"fr-other": true}

	tests := []struct {
		name      string
		peer      Claimant
		candidate Claimant
		ready     map[string]bool
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
			name:    "a bare spelling of the same file is the same claim",
			peer:    Claimant{ID: "fr-z64c", Status: "hooked", Assignee: "fractals/polecats/cantor", Paths: []string{"wanderHops.test.ts"}},
			want:    true,
			wantTxt: "fr-z64c",
		},
		{
			name:    "an open peer the board carries and the order reaches first holds it",
			peer:    Claimant{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T09:00:00Z", Paths: []string{claimTestFile}},
			want:    true,
			wantTxt: "comes first in dispatch order",
		},
		{
			name: "an open peer behind in dispatch order does not hold it",
			peer: Claimant{ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T20:00:00Z", Paths: []string{claimTestFile}},
		},
		{
			name: "an open peer the board does not carry does not hold it",
			// A labeled, blocked or deferred bead is never slung, so ranking
			// it first would hold the file for as long as its own hold lasts.
			peer:  Claimant{ID: "fr-z64c", Status: "open", Priority: 0, CreatedAt: "2026-10-31T09:00:00Z", Paths: []string{claimTestFile}},
			ready: boardHoldsNobody,
		},
		{
			name:    "a landed peer the candidate was filed before holds it",
			peer:    Claimant{ID: "fr-z64c", Status: "closed", Landed: true, ClosedAt: "2026-10-31T17:00:00Z", Paths: []string{claimTestFile}},
			want:    true,
			wantTxt: "landed after this bead was filed",
		},
		{
			name: "a landed peer the candidate was filed after does not hold it",
			// The author saw the landing; this is a follow-up, not a duplicate.
			peer: Claimant{ID: "fr-z64c", Status: "closed", Landed: true, ClosedAt: "2026-10-31T14:00:00Z", Paths: []string{claimTestFile}},
		},
		{
			name: "a landing older than the window does not hold it",
			peer: Claimant{ID: "fr-z64c", Status: "closed", Landed: true, ClosedAt: "2026-10-29T17:00:00Z", Paths: []string{claimTestFile}},
		},
		{
			name: "a closed bead that did not land does not hold it",
			peer: Claimant{ID: "fr-z64c", Status: "closed", ClosedAt: "2026-10-31T17:00:00Z", Paths: []string{claimTestFile}},
		},
		{
			name: "a filing time that cannot be read holds it, rather than passing a bead it cannot place",
			peer: Claimant{ID: "fr-z64c", Status: "closed", Landed: true, ClosedAt: "2026-10-31T17:00:00Z", Paths: []string{claimTestFile}},
			candidate: Claimant{ID: "fr-zdzs", Status: "open", Priority: 1,
				Paths: []string{claimTestFile}},
			want:    true,
			wantTxt: "landed after this bead was filed",
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
			ready := tc.ready
			if ready == nil {
				ready = boardHoldsFrZ64c
			}
			why, held := TargetClaimHold(c, []Claimant{tc.peer}, ready, claimTestNow)
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
