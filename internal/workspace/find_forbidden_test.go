package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// makeTown creates a directory with the primary workspace marker.
func makeTown(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(`{"type":"town","name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFind_ForbiddenRootResolvesToNothing(t *testing.T) {
	town := t.TempDir()
	makeTown(t, town)
	inner := filepath.Join(town, "rig", "polecats", "p", "repo", "internal", "pkg")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}

	// Without the guard the town resolves normally.
	if root, err := Find(inner); err != nil || root != town {
		t.Fatalf("Find without guard = %q, %v; want %q", root, err, town)
	}

	t.Setenv(EnvForbiddenTownRoot, town)
	root, err := Find(inner)
	if err != nil {
		t.Fatalf("Find with guard: %v", err)
	}
	if root != "" {
		t.Errorf("Find resolved forbidden town: %q", root)
	}
}

func TestFind_ForbiddenGuardRejectsInnerMarkersToo(t *testing.T) {
	town := t.TempDir()
	makeTown(t, town)
	// A rig inside the town has its own mayor/ directory (secondary marker);
	// the guard must not fall back to it.
	rig := filepath.Join(town, "gastown")
	makeTown(t, rig)
	inner := filepath.Join(rig, "internal", "pkg")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvForbiddenTownRoot, town)
	root, err := Find(inner)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if root != "" {
		t.Errorf("Find resolved a root inside the forbidden town: %q", root)
	}
}

func TestFind_ForbiddenGuardLeavesFixtureTownsAlone(t *testing.T) {
	town := t.TempDir()
	makeTown(t, town)

	t.Setenv(EnvForbiddenTownRoot, filepath.Join(t.TempDir(), "other-town"))
	root, err := Find(town)
	if err != nil || root != town {
		t.Errorf("Find = %q, %v; fixture town outside the forbidden root must resolve", root, err)
	}
}

func TestIsWorkspace_ForbiddenRoot(t *testing.T) {
	town := t.TempDir()
	makeTown(t, town)

	if ok, _ := IsWorkspace(town); !ok {
		t.Fatal("fixture town not recognized without guard")
	}
	t.Setenv(EnvForbiddenTownRoot, town)
	if ok, _ := IsWorkspace(town); ok {
		t.Error("IsWorkspace acknowledged the forbidden town")
	}
}
