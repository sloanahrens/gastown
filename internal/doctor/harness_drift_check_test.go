package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, rigPath string, files map[string]string) {
	t.Helper()
	m := harnessManifest{Files: files}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".gastown-harness-manifest.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestHarnessDriftCheck_NoManifest(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewHarnessDriftCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusSkipped {
		t.Fatalf("expected StatusSkipped (unknown) with no manifest, got %v: %s", result.Status, result.Message)
	}
	if result.Message != "no manifest" {
		t.Errorf("expected message 'no manifest', got %q", result.Message)
	}
}

func TestHarnessDriftCheck_Matches(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(tmpDir, rigName)
	scriptsDir := filepath.Join(rigPath, "scripts")
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		t.Fatal(err)
	}

	content := "#!/bin/bash\necho hi\n"
	if err := os.WriteFile(filepath.Join(scriptsDir, "om-gate.sh"), []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, rigPath, map[string]string{"scripts/om-gate.sh": sha256Hex(content)})

	check := NewHarnessDriftCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s\n%v", result.Status, result.Message, result.Details)
	}
}

func TestHarnessDriftCheck_HandEditDetected(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(tmpDir, rigName)
	scriptsDir := filepath.Join(rigPath, "scripts")
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		t.Fatal(err)
	}

	deployed := "#!/bin/bash\necho hi\n"
	writeManifest(t, rigPath, map[string]string{"scripts/om-gate.sh": sha256Hex(deployed)})

	// Hand edit: file on disk differs from what the manifest recorded.
	handEdited := deployed + "# oops, a manual tweak\n"
	if err := os.WriteFile(filepath.Join(scriptsDir, "om-gate.sh"), []byte(handEdited), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewHarnessDriftCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "scripts/om-gate.sh") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected details to name the drifted file, got %v", result.Details)
	}
}

func TestHarnessDriftCheck_MissingManagedFile(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, rigPath, map[string]string{"scripts/om-gate.sh": sha256Hex("anything")})

	check := NewHarnessDriftCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError for a missing managed file, got %v: %s", result.Status, result.Message)
	}
}
