package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetRigEntry_KeepsRigsAnotherWriterAdded is the registry-level half of
// gt-4iobv: a caller registering one rig must not drop the rigs another gt
// process registered or parked in the meantime.
func TestSetRigEntry_KeepsRigsAnotherWriterAdded(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rigs.json")

	// What the concurrent writers landed: a parked riga and a new rigb.
	if err := SetRigEntry(path, "riga", RigEntry{GitURL: "a", Parked: &RigParked{By: "op", Reason: "concurrent"}}); err != nil {
		t.Fatalf("SetRigEntry(riga): %v", err)
	}
	if err := SetRigEntry(path, "rigb", RigEntry{GitURL: "b"}); err != nil {
		t.Fatalf("SetRigEntry(rigb): %v", err)
	}

	// The caller's own rig, from a registry it read before those.
	if err := SetRigEntry(path, "rigc", RigEntry{GitURL: "c"}); err != nil {
		t.Fatalf("SetRigEntry(rigc): %v", err)
	}

	got, err := LoadRigsConfig(path)
	if err != nil {
		t.Fatalf("loading the registry: %v", err)
	}
	for _, name := range []string{"riga", "rigb", "rigc"} {
		if _, ok := got.Rigs[name]; !ok {
			t.Errorf("%s is missing from the registry: %v", name, got.Rigs)
		}
	}
	if p := got.Rigs["riga"].Parked; p == nil || p.Reason != "concurrent" {
		t.Errorf("riga's park record = %+v, want the record the concurrent park wrote", p)
	}
	if got.Version != CurrentRigsVersion {
		t.Errorf("version = %d, want a created registry to carry %d", got.Version, CurrentRigsVersion)
	}
}

func TestSetRigEntry_ReplacesTheSameRig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rigs.json")
	if err := SetRigEntry(path, "riga", RigEntry{GitURL: "old"}); err != nil {
		t.Fatalf("SetRigEntry(old): %v", err)
	}
	if err := SetRigEntry(path, "riga", RigEntry{GitURL: "new"}); err != nil {
		t.Fatalf("SetRigEntry(new): %v", err)
	}

	got, err := LoadRigsConfig(path)
	if err != nil {
		t.Fatalf("loading the registry: %v", err)
	}
	if len(got.Rigs) != 1 {
		t.Fatalf("registry = %v, want one entry", got.Rigs)
	}
	if got.Rigs["riga"].GitURL != "new" {
		t.Errorf("riga git_url = %q, want %q", got.Rigs["riga"].GitURL, "new")
	}
}

func TestDeleteRigEntry_KeepsOtherEntries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rigs.json")
	if err := SetRigEntry(path, "riga", RigEntry{GitURL: "a"}); err != nil {
		t.Fatalf("SetRigEntry(riga): %v", err)
	}
	if err := SetRigEntry(path, "rigb", RigEntry{GitURL: "b", Parked: &RigParked{By: "op", Reason: "concurrent"}}); err != nil {
		t.Fatalf("SetRigEntry(rigb): %v", err)
	}

	if err := DeleteRigEntry(path, "riga"); err != nil {
		t.Fatalf("DeleteRigEntry(riga): %v", err)
	}

	got, err := LoadRigsConfig(path)
	if err != nil {
		t.Fatalf("loading the registry: %v", err)
	}
	if _, ok := got.Rigs["riga"]; ok {
		t.Errorf("riga is still registered: %v", got.Rigs)
	}
	if p := got.Rigs["rigb"].Parked; p == nil || p.Reason != "concurrent" {
		t.Errorf("rigb = %+v, want it kept with the park record another writer made", got.Rigs["rigb"])
	}
}

// The file's bytes are the check: a delete with nothing to do must not
// rewrite the registry (a rewrite drops the formatting and any key this
// package's struct does not model).
func TestDeleteRigEntry_AbsentRigWritesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rigs.json")
	if err := SetRigEntry(path, "riga", RigEntry{GitURL: "a"}); err != nil {
		t.Fatalf("SetRigEntry(riga): %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: test-owned temp path
	if err != nil {
		t.Fatalf("reading the registry: %v", err)
	}

	if err := DeleteRigEntry(path, "rigz"); err != nil {
		t.Fatalf("DeleteRigEntry(rigz): %v", err)
	}

	after, err := os.ReadFile(path) //nolint:gosec // G304: test-owned temp path
	if err != nil {
		t.Fatalf("reading the registry: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("registry changed:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestDeleteRigEntry_MissingRegistryWritesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rigs.json")

	if err := DeleteRigEntry(path, "riga"); err != nil {
		t.Fatalf("DeleteRigEntry: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want the delete to leave no file behind", path, err)
	}
}
