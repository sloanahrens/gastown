package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRigsRegistryValidCheck_NoRigsJson(t *testing.T) {
	// No mayor/rigs.json at all — we could not validate anything, so this
	// must not report a clean StatusOK.
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir}

	check := NewRigsRegistryValidCheck()
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped when rigs.json is missing, got %v: %s", result.Status, result.Message)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
}

func TestRigsRegistryValidCheck_AllRigsExist(t *testing.T) {
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown"), 0755); err != nil {
		t.Fatal(err)
	}
	rigsJSON := `{"version":1,"rigs":{"gastown":{}}}`
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewRigsRegistryValidCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when all rigs exist, got %v: %s", result.Status, result.Message)
	}
}

func TestRigsRegistryValidCheck_MissingRig(t *testing.T) {
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	rigsJSON := `{"version":1,"rigs":{"ghost-rig":{}}}`
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewRigsRegistryValidCheck()
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for missing rig dir, got %v: %s", result.Status, result.Message)
	}
}
