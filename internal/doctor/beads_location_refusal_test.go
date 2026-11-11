package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A worktree redirect still naming the legacy mayor/rig/.beads, in a rig whose
// files are at <rig>/.beads: the stale-redirect check reports it as incorrect,
// with the rig-root path it expects.
func TestStaleBeadsRedirectCheck_LegacyWorktreeRedirectWarns(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
	crewBeads := filepath.Join(rigPath, "crew", "max", ".beads")
	writeBeadsLocationFiles(t, crewBeads, map[string]string{"redirect": "../../mayor/rig/.beads\n"})

	check := NewStaleBeadsRedirectCheck()
	got := check.Run(&CheckContext{TownRoot: tmpDir})
	if got.Status != StatusWarning {
		t.Fatalf("Run status = %v (%s), want StatusWarning for the legacy worktree redirect", got.Status, got.Message)
	}
	if !strings.Contains(strings.Join(got.Details, "\n"), `incorrect redirect: testrig/crew/max (has "../../mayor/rig/.beads"`) {
		t.Errorf("details = %v, want the legacy redirect named as incorrect", got.Details)
	}
}

// A rig whose beads are at <rig>/.beads with a redirect to a wrong target beside
// them: Run reports an error, Fix cannot change it, so Fix must not report success.
// A legacy redirect is only a warning and Fix leaves it, so that case returns nil.
func TestBeadsRedirectCheck_FixFailsWhenErrorRemains(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		redirect string
		wantErr  bool
	}{
		{name: "wrong target is an error Fix cannot change", redirect: "wrong/path\n", wantErr: true},
		{name: "legacy target is a warning Fix leaves alone", redirect: "mayor/rig/.beads\n", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			rigPath := filepath.Join(tmpDir, "testrig")
			writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
			writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), map[string]string{"redirect": tc.redirect})
			writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)

			check := NewBeadsRedirectCheck()
			ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
			if got := check.Run(ctx); got.Status == StatusOK {
				t.Fatalf("Run status = %v (%s), want a non-OK status for the stale redirect", got.Status, got.Message)
			}
			if err := check.Fix(ctx); (err != nil) != tc.wantErr {
				t.Fatalf("Fix error = %v, wantErr %v", err, tc.wantErr)
			}
			content, readErr := os.ReadFile(filepath.Join(rigPath, ".beads", "redirect"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(content) != tc.redirect {
				t.Errorf("redirect after Fix = %q, want it left as %q", content, tc.redirect)
			}
		})
	}
}

// Fix skips a legacy redirect by its typed flag, never by its reason text: a
// legacy entry is left alone whatever its reason says, and an untyped entry is
// not skipped for mentioning the legacy redirect.
func TestBeadsRedirectTargetCheck_FixSkipsOnlyTypedLegacy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		legacy  bool
		reason  string
		wantErr bool
	}{
		{name: "typed legacy entry is left alone whatever its reason says", legacy: true, reason: "reason reworded", wantErr: false},
		{name: "untyped entry is not skipped for mentioning the legacy redirect", legacy: false, reason: legacyRedirectReason + ": testrig/crew/max", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			rigPath := filepath.Join(tmpDir, "testrig")
			worktree := filepath.Join(rigPath, "crew", "max")
			writeBeadsLocationFiles(t, filepath.Join(worktree, ".beads"), map[string]string{"redirect": "../../missing/.beads\n"})

			check := NewBeadsRedirectTargetCheck()
			check.brokenTargets = []brokenTarget{{
				worktreePath: worktree,
				target:       "../../missing/.beads",
				resolvedPath: filepath.Join(rigPath, "missing", ".beads"),
				reason:       tc.reason,
				legacy:       tc.legacy,
			}}
			err := check.Fix(&CheckContext{TownRoot: tmpDir, RigName: "testrig"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Fix error = %v, wantErr %v", err, tc.wantErr)
			}
			content, readErr := os.ReadFile(filepath.Join(worktree, ".beads", "redirect"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(content) != "../../missing/.beads\n" {
				t.Errorf("worktree redirect after Fix = %q, want it left as it was", content)
			}
		})
	}
}

// A worktree with no redirect in a half-moved rig: StaleBeadsRedirectCheck
// expects the legacy copy, and Fix must refuse to write it back.
func TestStaleBeadsRedirectCheck_FixRefusesLegacyForMissingRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationNoDatabaseFiles)
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

// Two half-moved rigs are refused, and a healthy rig between them is still
// repaired: Fix collects each legacy refusal and reports every one at the end.
func TestStaleBeadsRedirectCheck_FixRepairsPastLegacyRefusal(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	for _, halfMoved := range []string{"aarig", "testrig"} {
		rigPath := filepath.Join(tmpDir, halfMoved)
		writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationNoDatabaseFiles)
		writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
		if err := os.MkdirAll(filepath.Join(rigPath, "crew", "max"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	healthyRig := filepath.Join(tmpDir, "healthy")
	writeBeadsLocationFiles(t, filepath.Join(healthyRig, ".beads"), beadsLocationRigFiles)
	if err := os.MkdirAll(filepath.Join(healthyRig, "crew", "max"), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewStaleBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	if got := check.Run(ctx); got.Status != StatusWarning {
		t.Fatalf("Run status = %v (%s), want StatusWarning for the missing redirects", got.Status, got.Message)
	}
	err := check.Fix(ctx)
	if err == nil {
		t.Fatal("Fix = nil, want an error naming both refused worktrees")
	}
	for _, want := range []string{"aarig/crew/max", "testrig/crew/max"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Fix error = %v, want it to name the refused %s", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(healthyRig, "crew", "max", ".beads", "redirect")); statErr != nil {
		t.Errorf("healthy rig's worktree was not repaired past the refusals: %v", statErr)
	}
}
