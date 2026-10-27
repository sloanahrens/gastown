package land

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/promptstamp"
)

// The prompt hash a review records is the one the author seat stamped on the
// work bead (gt-2lu00), and nothing else the bead's comments say about a
// prompt (gt-b9j3o.16). The stamp and its author are built with the spawn's
// own rule (promptstamp), so this test cannot pass while the landing reads a
// format no spawn writes.
func TestPromptStampSHATable(t *testing.T) {
	t.Parallel()
	polecat := Work{Rig: "gastown", Branch: "polecat/opal/gt-abc+x1"}
	// The spawn holds the stored prompt's whole sha256 and stamps its first
	// HashLen digits, as a real spawn does.
	const stored = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	seat := promptstamp.Author(polecat.Rig, "opal")
	stamp := promptstamp.Format(stored, 4096)
	for _, tc := range []struct {
		name     string
		work     Work
		comments []beads.Comment
		want     string
	}{
		{"the author seat's stamp", polecat,
			[]beads.Comment{{Author: seat, Text: stamp}}, stored[:promptstamp.HashLen]},
		{"the newest stamp, after a rework respawn stamped again", polecat,
			[]beads.Comment{
				{Author: seat, Text: promptstamp.Format(stored, 4096)},
				{Author: seat, Text: promptstamp.Format("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 4200)},
			}, "ffffffffffff"},
		{"a stamp another seat wrote names that seat's prompt", polecat,
			[]beads.Comment{{Author: promptstamp.Author(polecat.Rig, "corpus"), Text: stamp}}, ""},
		{"a stamp quoted inside a rejection note is no stamp", polecat,
			[]beads.Comment{{Author: seat, Text: "the prompt " + stamp + " scored low"}}, ""},
		{"a comment that is not a stamp", polecat,
			[]beads.Comment{{Author: seat, Text: "Submitted for landing: " + polecat.Branch}}, ""},
		{"a bead with no comments", polecat, nil, ""},
		{"a crew branch has no stored prompt", Work{Rig: "gastown", Branch: "crew/sloan/gt-abc"},
			[]beads.Comment{{Author: promptstamp.Author(polecat.Rig, "sloan"), Text: stamp}}, ""},
		{"a branch that names no seat", Work{Rig: "gastown", Branch: "sloan/gt-abc"},
			[]beads.Comment{{Author: promptstamp.Author(polecat.Rig, "sloan"), Text: stamp}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := promptStampSHA(tc.comments, tc.work); got != tc.want {
				t.Errorf("promptStampSHA(%+v) = %q, want %q", tc.comments, got, tc.want)
			}
		})
	}
}
