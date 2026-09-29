package beads

import (
	"reflect"
	"strings"
	"testing"
)

// beadsDirOf returns the BEADS_DIR a recorded call ran with.
func beadsDirOf(c recordedCall) string {
	dir := ""
	for _, kv := range c.env {
		if v, ok := strings.CutPrefix(kv, "BEADS_DIR="); ok {
			dir = v
		}
	}
	return dir
}

// TestAppendNotesRoutesByPrefix: AppendNotes on another database's issue runs
// bd against that database.
func TestAppendNotesRoutesByPrefix(t *testing.T) {
	t.Parallel()
	b, townBeads, rigBeads := routedTown(t)
	r := newRecorder(nil)
	b.exec = r.exec

	if err := b.AppendNotes("pt-x", "rig note"); err != nil {
		t.Fatal(err)
	}
	if err := b.AppendNotes("hq-y", "town note"); err != nil {
		t.Fatal(err)
	}
	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("%d bd calls, want 2", len(calls))
	}
	if got := beadsDirOf(calls[0]); got != rigBeads {
		t.Errorf("AppendNotes(pt-x) ran against %q, want the rig's %q", got, rigBeads)
	}
	if got := beadsDirOf(calls[1]); got != townBeads {
		t.Errorf("AppendNotes(hq-y) ran against %q, want the town's %q", got, townBeads)
	}
	if !reflect.DeepEqual(calls[0].args[len(calls[0].args)-4:], []string{"update", "pt-x", "--append-notes", "rig note"}) {
		t.Errorf("argv = %v", calls[0].args)
	}
}

// TestAppendEmptyNoteSendsNothing: bd update --append-notes "" appends a bare
// newline to existing notes, so AppendNotes sends bd nothing for an empty
// note.
func TestAppendEmptyNoteSendsNothing(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := NewIsolated(t.TempDir())
	b.exec = r.exec
	if err := b.AppendNotes("gt-x", ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls() {
		t.Errorf("AppendNotes(\"\") ran bd %v", c.args)
	}
}
