package taskmap

import (
	"strings"
	"testing"
)

// TestTrackerStatusCheckNamesPrintedReadyLine pins the tracker status Check to
// the line runTrackerStatus prints (internal/cmd/tracker_status.go
// writeTrackerStatusText): a label "Ready" padded to the value column, with no
// colon. A Check that names "Ready:" sends the reader to a line the screen
// never prints.
func TestTrackerStatusCheckNamesPrintedReadyLine(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("tracker status")
	if !ok {
		t.Fatal(`Lookup("tracker status") found no entry`)
	}
	if strings.Contains(e.Check, "Ready:") {
		t.Errorf("tracker status Check = %q; the screen prints a Ready label with no colon", e.Check)
	}
	if !strings.Contains(e.Check, "a Ready line") {
		t.Errorf("tracker status Check = %q, want it to name the printed Ready line", e.Check)
	}
}
