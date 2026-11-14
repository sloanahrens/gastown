package cmd

import (
	"strings"
	"testing"
)

// TestDoneHelpSaysDeferredReleasesTheBead: since gt-tq23e9 a DEFERRED exit
// returns the bead it paused to the open, unassigned pool for the next sling,
// so the exit-status help says that instead of the old "issue still open"
// (gt-mnl28v). The release is not unconditional: a parked, landing or
// other-actor-claimed bead is left as it is, so the entry names those cases.
func TestDoneHelpSaysDeferredReleasesTheBead(t *testing.T) {
	t.Parallel()
	var entry []string
	lines := strings.Split(doneCmd.Long, "\n")
	for i, l := range lines {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "DEFERRED" && f[1] == "-" {
			for _, next := range lines[i:] {
				if strings.TrimSpace(next) == "" {
					break
				}
				entry = append(entry, next)
			}
			break
		}
	}
	if len(entry) == 0 {
		t.Fatalf("gt done help has no DEFERRED exit-status line:\n%s", doneCmd.Long)
	}
	lower := strings.ToLower(strings.Join(entry, " "))
	for _, want := range []string{"released", "unassigned", "unless", "parked", "landing", "claimed by another"} {
		if !strings.Contains(lower, want) {
			t.Errorf("DEFERRED help entry does not say %q: %q", want, entry)
		}
	}
}
