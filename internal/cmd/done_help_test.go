package cmd

import (
	"strings"
	"testing"
)

// TestDoneHelpSaysDeferredReleasesTheBead: since gt-tq23e9 a DEFERRED exit
// returns the bead it paused to the open, unassigned pool for the next sling,
// so the exit-status help says that instead of the old "issue still open"
// (gt-mnl28v).
func TestDoneHelpSaysDeferredReleasesTheBead(t *testing.T) {
	t.Parallel()
	var line string
	for _, l := range strings.Split(doneCmd.Long, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "DEFERRED" && f[1] == "-" {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("gt done help has no DEFERRED exit-status line:\n%s", doneCmd.Long)
	}
	lower := strings.ToLower(line)
	for _, want := range []string{"released", "unassigned"} {
		if !strings.Contains(lower, want) {
			t.Errorf("DEFERRED help line does not say the bead is %s: %q", want, line)
		}
	}
}
