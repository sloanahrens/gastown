package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestTickEffRigDatabases: a rig's database is the dolt_database its metadata
// file records, not its name; a rig whose metadata names none, does not parse,
// or is absent is left out.
func TestTickEffRigDatabases(t *testing.T) {
	town := t.TempDir()
	writeMetadata := func(rig, content string) {
		t.Helper()
		dir := filepath.Join(town, rig, ".beads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata("gastown", `{"dolt_database":"gt"}`)
	writeMetadata("beads", `{"dolt_database":"be"}`)
	writeMetadata("third", `{}`)
	writeMetadata("broken", `{"dolt_database":`)

	got := rigDatabases(town, []string{"gastown", "beads", "third", "broken", "absent"})
	want := map[string]string{"gastown": "gt", "beads": "be"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rigDatabases = %v, want %v", got, want)
	}
}
