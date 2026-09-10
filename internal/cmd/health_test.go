package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeHealthMetadata writes a .beads/metadata.json with a dolt_database field,
// mirroring the layout doltserver.CollectDatabaseOwners scans.
func writeHealthMetadata(t *testing.T, beadsDir, doltDatabase string) {
	t.Helper()
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("creating beads dir %s: %v", beadsDir, err)
	}
	meta := map[string]interface{}{
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": doltDatabase,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshaling metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), data, 0644); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}
}

func writeHealthRigsJSON(t *testing.T, townRoot string, rigNames []string) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("creating mayor dir: %v", err)
	}
	rigs := make(map[string]interface{})
	for _, name := range rigNames {
		rigs[name] = map[string]interface{}{}
	}
	data, err := json.Marshal(map[string]interface{}{"rigs": rigs})
	if err != nil {
		t.Fatalf("marshaling rigs.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), data, 0644); err != nil {
		t.Fatalf("writing rigs.json: %v", err)
	}
}

// TestProductionDatabaseNames_MatchesRigsNotHardcodedList is a regression test
// for gt-lekn: `gt health` used to report a hardcoded {"hq", "gt", "mo"} list
// that invented a nonexistent "mo" database and omitted real rigs ("be", "om")
// whose names simply weren't in the hardcoded slice.
func TestProductionDatabaseNames_MatchesRigsNotHardcodedList(t *testing.T) {
	townRoot := t.TempDir()

	writeHealthRigsJSON(t, townRoot, []string{"gastown", "beads", "om"})
	writeHealthMetadata(t, filepath.Join(townRoot, ".beads"), "hq")
	writeHealthMetadata(t, filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"), "gt")
	writeHealthMetadata(t, filepath.Join(townRoot, "beads", "mayor", "rig", ".beads"), "be")
	writeHealthMetadata(t, filepath.Join(townRoot, "om", "mayor", "rig", ".beads"), "om")

	names := productionDatabaseNames(townRoot)

	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}

	for _, want := range []string{"hq", "gt", "be", "om"} {
		if !got[want] {
			t.Errorf("productionDatabaseNames(%v) missing real database %q", names, want)
		}
	}
	if got["mo"] {
		t.Errorf("productionDatabaseNames(%v) invented nonexistent database %q", names, "mo")
	}
	if len(names) == 0 || names[0] != "hq" {
		t.Errorf("productionDatabaseNames(%v) should list town database %q first", names, "hq")
	}
}
