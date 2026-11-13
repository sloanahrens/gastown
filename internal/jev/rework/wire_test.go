package rework

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Numstat, the call site, refuses a rig whose only git directory is a mayor/rig
// clone: the diff never runs there.
func TestTownRigsNumstatRefusesARigWithoutBareRepo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rigRoot := filepath.Join(town, "gastown")
	if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := &townRigs{townRoot: town}

	_, err := rigs.Numstat(context.Background(), "gastown", "main", "abc1234")

	want := "rig " + rigRoot + " has no .repo.git"
	if err == nil || err.Error() != want {
		t.Fatalf("Numstat err = %v, want %q", err, want)
	}
}
