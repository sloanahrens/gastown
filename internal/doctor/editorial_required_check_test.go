package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func setupRigWithRubric(t *testing.T, tmpDir, rigName string, editorialConfig string) string {
	t.Helper()
	rigPath := filepath.Join(tmpDir, rigName)
	rubricDir := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(rubricDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rubricDir, ".om.json"), []byte(`{"rubric":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if editorialConfig != "" {
		if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(editorialConfig), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return rigPath
}

func TestEditorialRequiredCheck_RubricPresentRequiredFalse(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	setupRigWithRubric(t, tmpDir, rigName, `{"merge_queue":{"editorial":{"required":false}}}`)

	check := NewEditorialRequiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", result.Status, result.Message)
	}
}

func TestEditorialRequiredCheck_RubricPresentNoConfigAtAll(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	setupRigWithRubric(t, tmpDir, rigName, "")

	check := NewEditorialRequiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning when required is unset, got %v: %s", result.Status, result.Message)
	}
}

func TestEditorialRequiredCheck_RubricPresentRequiredTrue(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	setupRigWithRubric(t, tmpDir, rigName, `{"merge_queue":{"editorial":{"required":true}}}`)

	check := NewEditorialRequiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s", result.Status, result.Message)
	}
}

func TestEditorialRequiredCheck_NoRubric(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewEditorialRequiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK when no rubric present, got %v: %s", result.Status, result.Message)
	}
}
