package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

const testRigsJSON = `{
  "rigs": {
    "gastown": {"beads": {"prefix": "-"}}
  }
}`

func writeRegistry(t *testing.T, townRoot, body string) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestRigsJSONCheck_RegistryWithPrefix_OK(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeRegistry(t, townRoot, testRigsJSON)

	check := NewRigsJSONCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Errorf("expected OK, got %s: %s", result.Status, result.Message)
	}
	if check.CanFix() {
		t.Error("the check restores nothing; CanFix() must be false")
	}
}

// TestRigsJSONCheck_NoPrefixes_Warning: a registry that loads but registers no
// prefix leaves PrefixRegistry empty, the silent-failure class this check
// guards (gt-y3pgh.2.8).
func TestRigsJSONCheck_NoPrefixes_Warning(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeRegistry(t, townRoot, `{"version":1,"rigs":{"gone":{"git_url":"x","added_at":"2026-01-01T00:00:00Z"}}}`)

	check := NewRigsJSONCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Errorf("expected Warning, got %s: %s", result.Status, result.Message)
	}
}

func TestRigsJSONCheck_Missing_Error(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	check := NewRigsJSONCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusError {
		t.Errorf("expected Error, got %s: %s", result.Status, result.Message)
	}
}

// TestRigsJSONCheck_TownRootCopy_Error: the town-root fallback copy is gone; a
// leftover copy is not a registry (gt-y3pgh.2.8).
func TestRigsJSONCheck_TownRootCopy_Error(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "rigs.json"), []byte(testRigsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewRigsJSONCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusError {
		t.Errorf("expected Error, got %s: %s", result.Status, result.Message)
	}
}
