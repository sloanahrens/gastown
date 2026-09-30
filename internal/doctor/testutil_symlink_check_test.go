package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewTestutilSymlinkCheck(t *testing.T) {
	t.Parallel()
	check := NewTestutilSymlinkCheck()

	if check.Name() != "testutil-symlink" {
		t.Errorf("expected name 'testutil-symlink', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestTestutilSymlinkCheck_NoRig(t *testing.T) {
	t.Parallel()
	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: t.TempDir(), RigName: ""}

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError when no rig, got %v", result.Status)
	}
}

func TestTestutilSymlinkCheck_NoCanonical(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning when canonical missing, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "canonical source missing") {
		t.Errorf("expected message about canonical source, got %q", result.Message)
	}
}

func TestTestutilSymlinkCheck_NoCrew(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "helper.go"), []byte("package testutil\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no crew, got %v: %s", result.Status, result.Message)
	}
}

func TestTestutilSymlinkCheck_CrewRealDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "helper.go"), []byte("package testutil\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create crew worker with real testutil directory (the drift problem)
	crewTestutil := filepath.Join(tmpDir, rigName, "crew", "alice", "internal", "testutil")
	if err := os.MkdirAll(crewTestutil, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crewTestutil, "helper.go"), []byte("package testutil\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for real dir, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "not symlinked") {
		t.Errorf("expected message about not symlinked, got %q", result.Message)
	}
	if len(result.Details) != 1 {
		t.Errorf("expected 1 detail, got %d", len(result.Details))
	}
	if !strings.Contains(result.Details[0], "crew/alice") {
		t.Errorf("expected detail to mention crew/alice, got %q", result.Details[0])
	}
}

// TestTestutilSymlinkCheck_LeftoverRefineryIgnored verifies that a leftover
// refinery/rig clone from the retired refinery role is not checked.
func TestTestutilSymlinkCheck_LeftoverRefineryIgnored(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}

	// Create leftover refinery/rig with real testutil directory
	refineryTestutil := filepath.Join(tmpDir, rigName, "refinery", "rig", "internal", "testutil")
	if err := os.MkdirAll(refineryTestutil, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for leftover refinery clone, got %v: %s %v", result.Status, result.Message, result.Details)
	}
}

func TestTestutilSymlinkCheck_NoInternalDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}

	// Create crew worker WITHOUT internal/ directory — should be skipped silently
	crewDir := filepath.Join(tmpDir, rigName, "crew", "newbie")
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when crew has no internal/, got %v: %s", result.Status, result.Message)
	}
}
