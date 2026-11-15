package taskmap

import (
	"strings"
	"testing"
)

// TestAwaitEventCheckNamesPrintedEventTypes pins the await-event Check to what
// internal/cmd/molecule_await_event.go prints (gt-xqb0yz): each received event
// is shown by its type, not by its file path, so the Check must name types.
func TestAwaitEventCheckNamesPrintedEventTypes(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("mol step await-event")
	if !ok {
		t.Fatal(`Lookup("mol step await-event") found no entry`)
	}
	if !strings.Contains(e.Check, "event types") {
		t.Errorf("mol step await-event Check = %q, want it to name the printed \"event types\"", e.Check)
	}
	if strings.Contains(e.Check, "event paths") {
		t.Errorf("mol step await-event Check = %q; it promises event paths the run does not print", e.Check)
	}
}
