package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// eventsJournalTown builds a town with hq and the named rigs, each with a
// store directory, and returns the town root and the stores' beads dirs.
func eventsJournalTown(t *testing.T, rigs ...string) (town string, dirs map[string]string) {
	t.Helper()
	town = t.TempDir()
	writeRigsFixture(t, town, rigs...)
	dirs = map[string]string{"hq": filepath.Join(town, ".beads")}
	for _, r := range rigs {
		dirs[r] = filepath.Join(town, r, "mayor", "rig", ".beads")
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return town, dirs
}

// TestEventsJournalCheck: a town with one store on, one off and one never set
// passes and names the two off stores in the details; bd is asked with
// gastown's own override removed and BEADS_DIR pinned to the store
// (gt-7iwy0.7).
func TestEventsJournalCheck(t *testing.T) {
	t.Parallel()
	town, dirs := eventsJournalTown(t, "alpha", "beta")

	bd := newFakeBD()
	check := NewEventsJournalCheck()
	check.environ = func() []string { return []string{"BD_EVENTS_JOURNAL=1", "BEADS_DIR=/elsewhere"} }
	_ = bd.db(filepath.Dir(dirs["hq"])).ConfigSet("events-journal", "true")
	_ = bd.db(filepath.Dir(dirs["alpha"])).ConfigSet("events-journal", "false")
	// beta never set: bd's default is off.

	res := check.Run(bd.ctx(town))
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message, "on in 1 of 3 store(s)") {
		t.Errorf("message = %q, want the count of stores on", res.Message)
	}
	joined := strings.Join(res.Details, "\n")
	if !strings.Contains(joined, "alpha: events-journal is off") {
		t.Errorf("details = %q, want alpha named as off", joined)
	}
	if !strings.Contains(joined, "beta: events-journal is not set") {
		t.Errorf("details = %q, want beta named as never set", joined)
	}
	if strings.Contains(joined, "hq:") {
		t.Errorf("details = %q, want the on store hq absent", joined)
	}
	for _, o := range bd.opened() {
		env := strings.Join(o.env, " ")
		if strings.Contains(env, "BD_EVENTS_JOURNAL") || !strings.Contains(env, "BEADS_DIR="+filepath.Join(o.dir, ".beads")) {
			t.Errorf("bd in %s ran with env %v", o.dir, o.env)
		}
	}
}

// TestEventsJournalCheckUnreadable: a store whose setting cannot be read fails
// the check by name with bd's output, and is not reported as off. A store
// that is off is still reported as state, not as a failure.
func TestEventsJournalCheckUnreadable(t *testing.T) {
	t.Parallel()
	town, dirs := eventsJournalTown(t, "alpha")

	bd := newFakeBD()
	check := NewEventsJournalCheck()
	_ = bd.db(filepath.Dir(dirs["hq"])).ConfigSet("events-journal", "false")
	bd.db(filepath.Dir(dirs["alpha"])).FailWith("config get events-journal", errors.New("database not found"))

	res := check.Run(bd.ctx(town))
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message, "in 1 store(s)") {
		t.Errorf("message = %q, want one unreadable store", res.Message)
	}
	joined := strings.Join(res.Details, "\n")
	if !strings.Contains(joined, "alpha: cannot read events-journal: database not found") {
		t.Errorf("details = %q, want alpha named with bd's output", joined)
	}
	if strings.Contains(joined, "alpha: events-journal is off") {
		t.Errorf("details = %q, want the unreadable store not reported as off", joined)
	}
	if !strings.Contains(joined, "hq: events-journal is off") {
		t.Errorf("details = %q, want hq's off state still reported", joined)
	}
}

// TestEventsJournalCheckUnloadableRegistry: an unreadable rigs registry is an
// error, never an empty set of stores that passes.
func TestEventsJournalCheckUnloadableRegistry(t *testing.T) {
	t.Parallel()
	bd := newFakeBD()
	res := NewEventsJournalCheck().Run(bd.ctx(t.TempDir()))
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
	if res.Message != "Could not load rigs registry" {
		t.Errorf("message = %q", res.Message)
	}
}
