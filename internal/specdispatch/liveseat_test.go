package specdispatch

import (
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// TestLiveSeatHold is the whole-token rule of gt-712i5k: a live sandbox
// record's branch names the bead its seat was spawned on, and only that bead.
// The longer ids are the mutation the rule has to survive: a substring match
// would hold a bead for gt-c8rbn5x, gt-c8rbn55 and its own child gt-c8rbn5.18.
func TestLiveSeatHold(t *testing.T) {
	t.Parallel()
	const seat = "gastown/polecats/warboy"
	const reason = seat + " still names it; the hold lifts when its sandbox record is gone"
	rec := func(branch string) session.SandboxRecord {
		return session.SandboxRecord{Session: "gt-warboy", Rig: "gastown", Polecat: "warboy", Branch: branch}
	}
	cases := []struct {
		name    string
		bead    string
		records []session.SandboxRecord
		want    string
	}{
		{
			name:    "a live record that names the bead holds it and names the seat",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("polecat/warboy/gt-c8rbn5+mwgjywwx")},
			want:    reason,
		},
		{
			name:    "a record naming another bead holds nothing",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("polecat/warboy/gt-other+mwgjywwx")},
		},
		{
			name:    "a longer id that starts with the bead id is another token",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("polecat/warboy/gt-c8rbn5x+mwgjywwx")},
		},
		{
			name:    "a longer id that ends with the bead id is another token",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("polecat/warboy/gt-c8rbn55+mwgjywwx")},
		},
		{
			name:    "a child id is another token",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("polecat/warboy/gt-c8rbn5.18+mwgjywwx")},
		},
		{
			name:    "the id inside a longer token is not the id",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("polecat/warboy/mwgt-c8rbn5+mwgjywwx")},
		},
		{
			name:    "an empty branch names nothing",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("")},
		},
		{
			name:    "an empty bead id is never named",
			bead:    "",
			records: []session.SandboxRecord{rec("polecat/warboy/+mwgjywwx")},
		},
		{
			name:    "the bead is named when it is one of several records",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("polecat/other/gt-aaaaa+1"), rec("polecat/warboy/gt-c8rbn5+mwgjywwx")},
			want:    reason,
		},
		{
			name: "the lowest seat is named when two records name the bead",
			bead: "gt-c8rbn5",
			records: []session.SandboxRecord{
				{Session: "gt-toast", Rig: "gastown", Polecat: "toast", Branch: "polecat/toast/gt-c8rbn5+a"},
				{Session: "gt-able", Rig: "gastown", Polecat: "able", Branch: "polecat/able/gt-c8rbn5+b"},
			},
			want: "gastown/polecats/able still names it; the hold lifts when its sandbox record is gone",
		},
		{
			name:    "the legacy separator ends the token too",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{rec("polecat/warboy/gt-c8rbn5@mwgjywwx")},
			want:    reason,
		},
		{
			name:    "a record that names no seat is named by its session",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{{Session: "gt-warboy", Branch: "polecat/warboy/gt-c8rbn5+mwgjywwx"}},
			want:    "gt-warboy still names it; the hold lifts when its sandbox record is gone",
		},
		{
			name:    "a record from another rig names its own seat",
			bead:    "gt-c8rbn5",
			records: []session.SandboxRecord{{Session: "fr-toast", Rig: "fractals", Polecat: "toast", Branch: "polecat/toast/gt-c8rbn5+a"}},
			want:    "fractals/polecats/toast still names it; the hold lifts when its sandbox record is gone",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, held := LiveSeatHold(tc.bead, tc.records)
			if held != (tc.want != "") {
				t.Fatalf("LiveSeatHold(%q) held = %v, want %v", tc.bead, held, tc.want != "")
			}
			if got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}
