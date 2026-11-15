package taskmap

import (
	"strings"
	"testing"
)

// TestPlacementUsesNameTheSituation pins the rule for the placements in
// place_p12bb.go (gt-vry3ku): a Use names the situation that calls for the
// command. The lease length, the file the Ring lives in, the subcommand menu,
// the verbs and the flags belong to the command's help, so a Use that restates
// any of them is refused.
func TestPlacementUsesNameTheSituation(t *testing.T) {
	t.Parallel()

	mechanism := []string{"36 hours", "mayor/town.json", "the verbs", "to sign", "to delete", "--force"}
	for _, path := range []string{"plugin sync", "ring", "ring forge", "ring remove"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			e, ok := Lookup(path)
			if !ok {
				t.Fatalf("Lookup(%q) found no entry", path)
			}
			if !strings.HasPrefix(e.Use, "Use this when") {
				t.Errorf("%s Use = %q; want a line starting \"Use this when\"", path, e.Use)
			}
			for _, word := range mechanism {
				if strings.Contains(e.Use, word) {
					t.Errorf("%s Use = %q; it names the mechanism %q, not the situation that calls for the command", path, e.Use, word)
				}
			}
		})
	}
}
