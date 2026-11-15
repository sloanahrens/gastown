package taskmap

import (
	"strings"
	"testing"
)

// TestAwaitSignalSitsWithDispatch pins the grouping judgement place_p6d.go
// records (gt-f9qwcc): an agent runs gt mol await-signal by hand inside a
// patrol loop, so the internal rule for gt's own plumbing does not fit, and it
// belongs in dispatch with the hook commands it waits on activity for.
func TestAwaitSignalSitsWithDispatch(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("mol await-signal")
	if !ok {
		t.Fatal(`Lookup("mol await-signal") found no entry`)
	}
	if e.Group != "dispatch" {
		t.Errorf("mol await-signal Group = %q, want \"dispatch\": an agent runs it by hand, not gt's plumbing", e.Group)
	}
}

// TestAwaitSignalCheckNamesPrintedTimeoutLines pins the await-signal Check to
// the lines internal/cmd/molecule_await_signal.go prints (gt-f9qwcc): every run
// ends in "Signal received", or in one of two Timeout forms depending on
// whether --agent-bead was given.
func TestAwaitSignalCheckNamesPrintedTimeoutLines(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("mol await-signal")
	if !ok {
		t.Fatal(`Lookup("mol await-signal") found no entry`)
	}
	for _, printed := range []string{"Signal received", "idle cycle", "(no activity)"} {
		if !strings.Contains(e.Check, printed) {
			t.Errorf("mol await-signal Check = %q, want it to name the printed %q", e.Check, printed)
		}
	}
}

// TestBurnCheckNamesPrintedBurnLines pins the burn Check to what
// internal/cmd/molecule_lifecycle.go prints (gt-f9qwcc): the run reports the
// burned molecule, or says there is nothing to burn. The hook state after the
// run is not part of the output, so the Check does not promise it.
func TestBurnCheckNamesPrintedBurnLines(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("mol burn")
	if !ok {
		t.Fatal(`Lookup("mol burn") found no entry`)
	}
	for _, printed := range []string{"Burned molecule", "nothing to burn"} {
		if !strings.Contains(e.Check, printed) {
			t.Errorf("mol burn Check = %q, want it to name the printed %q", e.Check, printed)
		}
	}
	if strings.Contains(e.Check, "gt hook") {
		t.Errorf("mol burn Check = %q; it promises a gt hook result the run does not print", e.Check)
	}
}
