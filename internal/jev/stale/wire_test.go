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
		{"landed on main", false},
		{"superseded by gt-a1", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsLandedReason(tt.reason); got != tt.want {
			t.Errorf("IsLandedReason(%q) = %v, want %v", tt.reason, got, tt.want)
		}
	}
}
