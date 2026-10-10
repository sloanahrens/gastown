package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corruptRigsTown writes a town whose registry will not parse and returns its
// root. Only mayor/rigs.json matters to the commands under test.
func corruptRigsTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	path := filepath.Join(town, "mayor", "rigs.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"rigs":{`), 0o600); err != nil {
		t.Fatal(err)
	}
	return town
}

// TestReadyFailsOnCorruptRigsJSON: gt ready must not fall back to the
// town-only view and exit 0 when rigs.json is damaged; the unreadable
// registry is an error (gt-52mgl).
func TestReadyFailsOnCorruptRigsJSON(t *testing.T) {
	t.Parallel()

	err := runReadyIn(corruptRigsTown(t))
	if err == nil {
		t.Fatal("runReady with a corrupt rigs.json = nil, want an error")
	}
	if !strings.Contains(err.Error(), "rigs config") {
		t.Errorf("runReady error = %v, want it to name the rigs config", err)
	}
}
