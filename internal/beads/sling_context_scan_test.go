package beads

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSlingContextSearchDirs_TownRigsAndMayorRigsOnly(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, dir := range []string{
		"gastown/.beads",         // a rig with its own store
		"beads/mayor/rig/.beads", // a rig whose store is under mayor/rig
		"norig",                  // no store: skipped
		".hidden/.beads",         // dot dirs are skipped
		"mayor/.beads",           // the mayor's dir is skipped
		"settings/.beads",        // so is settings
	} {
		if err := os.MkdirAll(filepath.Join(town, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := SlingContextSearchDirs(town)
	if err != nil {
		t.Fatal(err)
	}
	// os.ReadDir sorts by name, so the order is stable.
	want := []string{
		town,
		filepath.Join(town, "beads", "mayor", "rig"),
		filepath.Join(town, "gastown"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SlingContextSearchDirs = %v, want %v", got, want)
	}
}

func TestAreScheduled_UnlistableTownFailsClosed(t *testing.T) {
	t.Parallel()
	ids := []string{"gt-a", "gt-b"}
	got := AreScheduled(filepath.Join(t.TempDir(), "missing"), ids)
	for _, id := range ids {
		if !got[id] {
			t.Errorf("AreScheduled reported %s unscheduled when contexts could not be listed; a caller would dispatch on a guess", id)
		}
	}
	if len(AreScheduled("unused", nil)) != 0 {
		t.Error("AreScheduled with no IDs must report nothing")
	}
}
