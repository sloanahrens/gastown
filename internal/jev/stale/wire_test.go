package stale

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestLandedAcceptsLandedOnReason covers the close reasons a landing writes.
// The landing worker closes a bead with "Landed on <target>" plus its record,
// so the stale pass must offer that bead as landed evidence, not only the bare
// word the molecule close writes.
func TestLandedAcceptsLandedOnReason(t *testing.T) {
	t.Parallel()
	closed := []*beads.Issue{
		{ID: "gt-molecule", CloseReason: "landed"},
		{ID: "gt-record", CloseReason: "Landed on main\ntarget_branch: main\ncommit_sha: abc123\nlanded_commit: abc123\npatch_id: p1"},
		{ID: "gt-wontfix", CloseReason: "wontfix: the rewrite replaced it"},
		{ID: "gt-open-reason", CloseReason: ""},
	}

	var got []string
	for _, b := range landedAmong(closed) {
		got = append(got, b.ID)
	}
	want := []string{"gt-molecule", "gt-record"}
	if len(got) != len(want) {
		t.Fatalf("landedAmong = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("landedAmong = %v, want %v", got, want)
		}
	}
}

// ClosingVerdict is the one reading of a close reason: the stale pass, the
// scorecard's stale and dispatch batteries and the attention battery all take
// what it says, so a reason cannot be landed to one of them and superseded to
// another. A person's wording is matched whatever its case, and the reason's
// first line decides: detail under it can quote a landing record without
// making the bead landed.
func TestClosingVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		want   Verdict
	}{
		{"landed", VerdictLanded},
		{"  landed\n", VerdictLanded},
		{"Landed", VerdictLanded},
		{"Landed on main", VerdictLanded},
		{"Landed on main\ntarget_branch: main", VerdictLanded},
		{"  Landed on main", VerdictLanded},
		{"landed on main", VerdictLanded},
		{"Landed on main\nlanded_commit: c0ffee\npatch_id: p1", VerdictLanded},
		{"landed\npatch_id: p1", VerdictLanded},
		{"landed_commit: c0ffee\npatch_id: p1", VerdictLanded},
		{"closed: landed_commit: c0ffee", VerdictNone},
		{"superseded by gt-a1", VerdictSuperseded},
		{"Superseded by gt-a2", VerdictSuperseded},
		{"duplicate of gt-a3", VerdictSuperseded},
		{"Duplicate of gt-a4", VerdictSuperseded},
		// A superseding close says what the landing it replaces landed as;
		// that quote is detail, and the verb line still decides.
		{"superseded by gt-a5\nlanded_commit: c0ffee", VerdictSuperseded},
		{"duplicate of gt-a6\nLanded on main\nlanded_commit: c0ffee", VerdictSuperseded},
		// So does a landing that mentions an obsolete copy below its verb
		// line; the verb line still decides.
		{"Landed on main\nnote: the obsolete duplicate went too", VerdictLanded},
		{"wontfix: the rewrite replaced it", VerdictNone},
		{"", VerdictNone},
	}
	for _, tt := range tests {
		if got := ClosingVerdict(tt.reason); got != tt.want {
			t.Errorf("ClosingVerdict(%q) = %v, want %v", tt.reason, got, tt.want)
		}
	}
}

func TestIsLandedReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		want   bool
	}{
		{"landed", true},
		{"  landed\n", true},
		{"Landed", true},
		{"Landed on main", true},
		{"Landed on main\ntarget_branch: main", true},
		{"  Landed on main", true},
		{"landed on main", true},
		{"landed\npatch_id: p1", true},
		{"superseded by gt-a1", false},
		{"duplicate of gt-a1", false},
		{"superseded by gt-a1\nlanded_commit: c0ffee", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsLandedReason(tt.reason); got != tt.want {
			t.Errorf("IsLandedReason(%q) = %v, want %v", tt.reason, got, tt.want)
		}
	}
}
