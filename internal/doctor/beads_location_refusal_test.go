package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A worktree with no redirect in a half-moved rig: StaleBeadsRedirectCheck
// expects the legacy copy, and Fix must refuse to write it back.
func TestStaleBeadsRedirectCheck_FixRefusesLegacyForMissingRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
	writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
	crewDir := filepath.Join(rigPath, "crew", "max")
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewStaleBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	if got := check.Run(ctx); got.Status != StatusWarning {
		t.Fatalf("Run status = %v (%s), want StatusWarning for the missing redirect", got.Status, got.Message)
	}
	err := check.Fix(ctx)
	if err == nil || !strings.Contains(err.Error(), "refusing a redirect to the legacy mayor/rig/.beads") {
		t.Fatalf("Fix error = %v, want a refusal naming the legacy mayor/rig/.beads", err)
	}
	if _, statErr := os.Stat(filepath.Join(crewDir, ".beads", "redirect")); statErr == nil {
		t.Error("Fix wrote a redirect to the legacy mayor/rig/.beads")
	}
}
