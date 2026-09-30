package doltserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBeadsDirForDatabase: the reaper writes through bd, and bd picks its
// database from a beads dir, so a database name the reaper discovered on the
// server must map back to the beads dir whose metadata.json names it.
func TestBeadsDirForDatabase(t *testing.T) {
	townRoot := t.TempDir()
	setupRigsJSON(t, townRoot, []string{"gastown", "myrig"})
	setupRigMetadata(t, townRoot, "hq", "hq")
	setupRigMetadata(t, townRoot, "gastown", "gt")
	setupRigMetadata(t, townRoot, "myrig", "custom_db")

	// A rig reachable only through routes.jsonl.
	routed := filepath.Join(townRoot, "hop", ".beads")
	if err := os.MkdirAll(routed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routed, "metadata.json"), []byte(`{"dolt_database":"hop"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(`{"prefix":"hop-","path":"hop"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for db, want := range map[string]string{
		"hq":        filepath.Join(townRoot, ".beads"),
		"gt":        filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"),
		"custom_db": filepath.Join(townRoot, "myrig", "mayor", "rig", ".beads"),
		"hop":       routed,
	} {
		got, err := BeadsDirForDatabase(townRoot, db)
		if err != nil {
			t.Errorf("BeadsDirForDatabase(%q): %v", db, err)
			continue
		}
		if got != want {
			t.Errorf("BeadsDirForDatabase(%q) = %q, want %q", db, got, want)
		}
	}

	// The rig NAME is not the database: a query for it must not resolve.
	if got, err := BeadsDirForDatabase(townRoot, "gastown"); err == nil {
		t.Errorf("BeadsDirForDatabase(rig name) = %q, want an error: gastown's database is gt", got)
	}
	if _, err := BeadsDirForDatabase(townRoot, "orphan_db"); err == nil || !strings.Contains(err.Error(), "orphan_db") {
		t.Errorf("unmapped database error = %v, want one naming the database", err)
	}
}
