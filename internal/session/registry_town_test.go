package session

import (
	"os"
	"path/filepath"
	"testing"
)

const testRigsJSON = `{
  "rigs": {
    "gastown": {"beads": {"prefix": "-"}},
    "beads":   {"beads": {"prefix": "bd-"}}
  }
}`

// TestBuildPrefixRegistryFromTown_CanonicalExists: mayor/rigs.json is the
// registry, and nothing is copied to the town root (gt-y3pgh.2.8).
func TestBuildPrefixRegistryFromTown_CanonicalExists(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(mayorDir, "rigs.json")
	if err := os.WriteFile(canonical, []byte(testRigsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	r, err := BuildPrefixRegistryFromTown(townRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rig := r.RigForPrefix("-"); rig != "gastown" {
		t.Errorf("expected gastown for prefix -, got %q", rig)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "rigs.json")); !os.IsNotExist(err) {
		t.Errorf("town-root rigs.json exists (%v); the fallback copy is deleted (gt-y3pgh.2.8)", err)
	}
}

// TestBuildPrefixRegistryFromTown_TownRootCopyIgnored: a leftover town-root
// rigs.json registers nothing — only the config loaders read the registry
// (gt-y3pgh.2.8).
func TestBuildPrefixRegistryFromTown_TownRootCopyIgnored(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "rigs.json"), []byte(testRigsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	r, err := BuildPrefixRegistryFromTown(townRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rig := r.RigForPrefix("bd-"); rig != "bd-" {
		t.Errorf("RigForPrefix(bd-) = %q, want the unknown-prefix fallthrough", rig)
	}
}

func TestBuildPrefixRegistryFromTown_BothMissing_EmptyRegistry(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// No rigs.json anywhere.

	r, err := BuildPrefixRegistryFromTown(townRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Registry should be empty — RigForPrefix returns the prefix itself when unknown.
	if rig := r.RigForPrefix("-"); rig != "-" {
		t.Errorf("expected fallthrough prefix -, got %q", rig)
	}
	// Verify no rigs were registered by checking a known rig name returns default.
	if prefix := r.PrefixForRig("gastown"); prefix != DefaultPrefix {
		t.Errorf("expected default prefix for unknown rig, got %q", prefix)
	}
}

// TestBuildPrefixRegistryFromTown_TwoFileLayout: after gt config migrate the
// registry is a section of mayor/town.json; it is read there, and no
// rigs.json is written anywhere.
func TestBuildPrefixRegistryFromTown_TwoFileLayout(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	town := `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","registry":` + testRigsJSON + `}`
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(town), 0644); err != nil {
		t.Fatal(err)
	}
	r, err := BuildPrefixRegistryFromTown(townRoot)
	if err != nil {
		t.Fatal(err)
	}
	if rig := r.RigForPrefix("bd-"); rig != "beads" {
		t.Errorf("RigForPrefix(bd-) = %q, want beads", rig)
	}
	for _, p := range []string{filepath.Join(mayorDir, "rigs.json"), filepath.Join(townRoot, "rigs.json")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s written on the two-file layout (%v)", p, err)
		}
	}
}
