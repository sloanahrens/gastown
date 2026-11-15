package taskmap

import (
	"strings"
	"testing"
)

// TestSlotRunCheckQuotesPrintedOutput pins the slot run Check to what the run
// and gt slot status print: the acquire line in internal/slot/run.go, and the
// held-by line that internal/cmd/slot.go prints while the slot is held. The
// run prints nothing on release, so the Check must not promise a listing once
// it releases.
func TestSlotRunCheckQuotesPrintedOutput(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("slot run")
	if !ok {
		t.Fatal("Lookup(slot run) found no entry")
	}
	for _, printed := range []string{
		"Container-gate slot acquired",
		"held by",
	} {
		if !strings.Contains(e.Check, printed) {
			t.Errorf("slot run Check = %q, want it to quote the printed %q", e.Check, printed)
		}
	}
	if strings.Contains(e.Check, "once it releases") {
		t.Errorf("slot run Check = %q; it promises a status listing after release, which nothing prints", e.Check)
	}
}
