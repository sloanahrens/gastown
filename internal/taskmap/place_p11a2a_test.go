package taskmap

import (
	"strings"
	"testing"
)

// TestCrewStaleUseStatesTheFilter pins the crew stale Use to the filter
// crewfresh.Stale applies (gt-io9yja): a clone is listed when it has not
// fetched within a day, or when a read of it failed, whether or not it is
// behind origin/main. A Use that promises only clones missing commits sends
// the reader looking for a list the command never prints.
func TestCrewStaleUseStatesTheFilter(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("crew stale")
	if !ok {
		t.Fatal(`Lookup("crew stale") found no entry`)
	}
	if !strings.HasPrefix(e.Use, "Use this to ") {
		t.Errorf("crew stale Use = %q; want a line starting \"Use this to \"", e.Use)
	}
	for _, wrong := range []string{"missing commits", "behind origin/main"} {
		if strings.Contains(e.Use, wrong) {
			t.Errorf("crew stale Use = %q; a clone need not be behind origin/main to be listed, so the Use should not claim %q", e.Use, wrong)
		}
	}
	if !strings.Contains(e.Use, "could not be read") {
		t.Errorf("crew stale Use = %q; an unreadable clone is listed whatever its fetch age, so the Use should say so", e.Use)
	}
}
