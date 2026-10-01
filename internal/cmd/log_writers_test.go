package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gtevents "github.com/steveyegge/gastown/internal/events"
)

// readEventLine reports the single event a writer appended to townRoot's events
// log. A writer that appends none, or more than one, fails the caller.
func readEventLine(t *testing.T, townRoot string) gtevents.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(townRoot, gtevents.EventsFile))
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	lines := splitNonEmpty(string(data))
	if len(lines) != 1 {
		t.Fatalf("wrote %d event lines, want 1: %s", len(lines), data)
	}
	var event gtevents.Event
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	return event
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestLogSessionWakeWritesWakeEvent covers the record `gt session start` and
// `gt session restart` used to write to town.log: the events log carries it now
// (gt-i057g), which is what keeps a restarted session attributable (gt-tcrgb).
func TestLogSessionWakeWritesWakeEvent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	if err := logSessionWake(townRoot, "gastown/shale", "gastown", "restart requested by daemon/patrol-scan"); err != nil {
		t.Fatalf("logSessionWake: %v", err)
	}

	event := readEventLine(t, townRoot)
	if event.Type != gtevents.TypeWake {
		t.Errorf("type = %q, want %q", event.Type, gtevents.TypeWake)
	}
	if event.Actor != "gastown/shale" {
		t.Errorf("actor = %q, want gastown/shale", event.Actor)
	}
	if event.Visibility != gtevents.VisibilityFeed {
		t.Errorf("visibility = %q, want %q", event.Visibility, gtevents.VisibilityFeed)
	}
	if got, _ := event.Payload["context"].(string); got != "restart requested by daemon/patrol-scan" {
		t.Errorf("payload context = %#v", event.Payload["context"])
	}
}

// TestLogSessionKillWritesKillEvent covers the record `gt session stop` and
// `gt crew stop` used to write to town.log (gt-i057g).
func TestLogSessionKillWritesKillEvent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	if err := logSessionKill(townRoot, "gastown/crew/max", "gastown", "max", "gt crew stop"); err != nil {
		t.Fatalf("logSessionKill: %v", err)
	}

	event := readEventLine(t, townRoot)
	if event.Type != gtevents.TypeKill {
		t.Errorf("type = %q, want %q", event.Type, gtevents.TypeKill)
	}
	if event.Actor != "gastown/crew/max" {
		t.Errorf("actor = %q, want gastown/crew/max", event.Actor)
	}
	if got, _ := event.Payload["reason"].(string); got != "gt crew stop" {
		t.Errorf("payload reason = %#v", event.Payload["reason"])
	}
}

// TestLogCrashExitClassesAreAuditOnlyExceptCrashes covers the pane-died hook's
// three exit classes now that events is the only log (gt-i057g). A crash is a
// feed event, as it always was; a normal or interrupted exit is recorded for
// `gt log` without adding a session_death to the feed for every clean session
// end.
func TestLogCrashExitClassesAreAuditOnlyExceptCrashes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		exitCode   int
		wantReason string
		wantVis    string
	}{
		{name: "normal exit", exitCode: 0, wantReason: "exited normally", wantVis: gtevents.VisibilityAudit},
		{name: "interrupted", exitCode: 130, wantReason: "interrupted (exit 130)", wantVis: gtevents.VisibilityAudit},
		{name: "crash", exitCode: 42, wantReason: "crashed with exit code 42", wantVis: gtevents.VisibilityFeed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()

			if err := logCrash(townRoot, "gastown/polecats/rust", "gt-gastown-rust", tc.exitCode); err != nil {
				t.Fatalf("logCrash(%d): %v", tc.exitCode, err)
			}

			event := readEventLine(t, townRoot)
			if event.Type != gtevents.TypeSessionDeath {
				t.Errorf("type = %q, want %q", event.Type, gtevents.TypeSessionDeath)
			}
			if event.Visibility != tc.wantVis {
				t.Errorf("visibility = %q, want %q", event.Visibility, tc.wantVis)
			}
			if got, _ := event.Payload["reason"].(string); got != tc.wantReason {
				t.Errorf("payload reason = %#v, want %q", event.Payload["reason"], tc.wantReason)
			}
		})
	}
}
