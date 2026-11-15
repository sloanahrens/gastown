package taskmap

import (
	"strings"
	"testing"
)

// TestSessionListUseNamesStoppedSessions pins the session list Use to what
// `gt session list` prints (internal/cmd/session.go runSessionList): it lists
// every polecat seat, stopped ones included, marked with a hollow dot. A Use
// that says only running sessions are listed sends the reader past a seat that
// is there.
func TestSessionListUseNamesStoppedSessions(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("session list")
	if !ok {
		t.Fatal(`Lookup("session list") found no entry`)
	}
	if !strings.Contains(e.Use, "stopped") {
		t.Errorf("session list Use = %q, want it to say stopped sessions are listed too", e.Use)
	}
	if strings.Contains(e.Use, "running now") {
		t.Errorf("session list Use = %q; it promises only running sessions, but the run lists stopped ones too", e.Use)
	}
}

// TestSessionListCheckQuotesPrintedOutput pins the session list Check to the
// text runSessionList prints: the Active Sessions heading, a filled dot for a
// running session and a hollow dot for a stopped one, and two lines per session
// (the rig/polecat line, then the session id line).
func TestSessionListCheckQuotesPrintedOutput(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("session list")
	if !ok {
		t.Fatal(`Lookup("session list") found no entry`)
	}
	for _, printed := range []string{
		"Active Sessions",
		"●",
		"○",
		"two lines per session",
	} {
		if !strings.Contains(e.Check, printed) {
			t.Errorf("session list Check = %q, want it to quote the printed %q", e.Check, printed)
		}
	}
	if strings.Contains(e.Check, "one line per session") {
		t.Errorf("session list Check = %q; the run prints two lines per session, not one", e.Check)
	}
}
