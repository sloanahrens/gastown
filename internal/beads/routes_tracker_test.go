package beads

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRigOnTracker: a rig is on the tracker when its route names that backend,
// off it when the route names another or the table names no such rig, and a
// table that cannot be read is an error rather than a rig with nothing to
// revoke.
func TestRigOnTracker(t *testing.T) {
	town := t.TempDir()
	beadsDir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	table := `{"prefix":"gt-","path":"gastown/mayor/rig","backend":"tracker"}
{"prefix":"bd-","path":"beads/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(beadsDir, RoutesFileName), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		rig  string
		want bool
	}{
		{"a tracker rig", "gastown", true},
		{"a Dolt rig", "beads", false},
		{"a rig the table does not name", "other", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RigOnTracker(town, tt.rig)
			if err != nil {
				t.Fatalf("RigOnTracker(%q) error = %v", tt.rig, err)
			}
			if got != tt.want {
				t.Errorf("RigOnTracker(%q) = %v, want %v", tt.rig, got, tt.want)
			}
		})
	}

	// A town with no route table names no tracker rig.
	if got, err := RigOnTracker(t.TempDir(), "gastown"); err != nil || got {
		t.Errorf("RigOnTracker with no route table = %v, %v; want false, nil", got, err)
	}

	// routes.jsonl that is a directory cannot be read.
	if err := os.RemoveAll(filepath.Join(beadsDir, RoutesFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(beadsDir, RoutesFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RigOnTracker(town, "gastown"); err == nil {
		t.Error("RigOnTracker read an unreadable route table as a rig off the tracker")
	}
}
