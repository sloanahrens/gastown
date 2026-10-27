package land

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The prompt hash a review records is the one the author seat stamped on the
// work bead (gt-2lu00), and nothing else the bead's comments say about a
// prompt (gt-b9j3o.16).
func TestPromptStampSHATable(t *testing.T) {
	t.Parallel()
	polecat := Work{Rig: "gastown", Branch: "polecat/opal/gt-abc+x1"}
	stamp := "prompt 0123456789ab (4096 bytes)"
	for _, tc := range []struct {
		name     string
		work     Work
		comments []beads.Comment
		want     string
	}{
		{"the author seat's stamp", polecat,
			[]beads.Comment{{Author: "gastown/polecats/opal", Text: stamp}}, "0123456789ab"},
		{"the newest stamp, after a rework respawn stamped again", polecat,
			[]beads.Comment{
				{Author: "gastown/polecats/opal", Text: "prompt 0123456789ab (4096 bytes)"},
				{Author: "gastown/polecats/opal", Text: "prompt ffffffffffff (4200 bytes)"},
			}, "ffffffffffff"},
		{"a stamp another seat wrote names that seat's prompt", polecat,
			[]beads.Comment{{Author: "gastown/polecats/corpus", Text: stamp}}, ""},
		{"a stamp quoted inside a rejection note is no stamp", polecat,
			[]beads.Comment{{Author: "gastown/polecats/opal", Text: "the prompt " + stamp + " scored low"}}, ""},
		{"a comment that is not a stamp", polecat,
			[]beads.Comment{{Author: "gastown/polecats/opal", Text: "Submitted for landing: " + polecat.Branch}}, ""},
		{"a bead with no comments", polecat, nil, ""},
		{"a crew branch has no stored prompt", Work{Rig: "gastown", Branch: "crew/sloan/gt-abc"},
			[]beads.Comment{{Author: "gastown/polecats/sloan", Text: stamp}}, ""},
		{"a branch that names no seat", Work{Rig: "gastown", Branch: "sloan/gt-abc"},
			[]beads.Comment{{Author: "gastown/polecats/sloan", Text: stamp}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := promptStampSHA(tc.comments, tc.work); got != tc.want {
				t.Errorf("promptStampSHA(%+v) = %q, want %q", tc.comments, got, tc.want)
			}
		})
	}
}
