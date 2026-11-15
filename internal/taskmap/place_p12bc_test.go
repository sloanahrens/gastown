package taskmap

import (
	"strings"
	"testing"
)

// TestSlotReapCheckQuotesPrintedOutput pins the slot reap Check to the clause
// internal/cmd/slot.go prints when nothing is left to reap: "No gate debris
// older than <window>, and no container whose owning test process is gone."
// A Check that names words the output never prints sends the reader looking
// for a line that is not there (gt-3ku6x2).
func TestSlotReapCheckQuotesPrintedOutput(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("slot reap")
	if !ok {
		t.Fatal(`Lookup("slot reap") found no entry`)
	}
	for _, printed := range []string{
		"No gate debris older than",
		"and no container whose owning test process is gone",
	} {
		if !strings.Contains(e.Check, printed) {
			t.Errorf("slot reap Check = %q, want it to quote the printed %q", e.Check, printed)
		}
	}
	if strings.Contains(e.Check, "once no suite holds the slot") {
		t.Errorf("slot reap Check = %q; it names a clause the run never prints", e.Check)
	}
}
