package doctor

import (
	"testing"
)

// TestHooksLiveFireCheck_Metadata sanity-checks the check's static
// properties: it is not auto-fixable (only investigation helps), and it
// belongs to the Hooks category.
func TestHooksLiveFireCheck_Metadata(t *testing.T) {
	check := NewHooksLiveFireCheck()
	if check.CanFix() {
		t.Error("hooks-live-fire should not claim to be auto-fixable")
	}
	if check.Name() != "hooks-live-fire" {
		t.Errorf("unexpected check name: %q", check.Name())
	}
	if err := check.Fix(&CheckContext{}); err == nil {
		t.Error("Fix should always return an error (no mechanical fix exists)")
	}
}

// TestHooksLiveFireCheck_NoPolecatSettings pins the "never StatusOK on infra
// error" contract for the case where no polecat settings.json exists yet to
// test against: it must report StatusWarning (inconclusive), never
// StatusOK. This is the same requirement the bead (gt-5ihs) named
// explicitly — earlier verification methods gave a false pass; this check
// must fail toward "inconclusive", never toward "pass", when it can't
// actually exercise the real dispatch path.
func TestHooksLiveFireCheck_NoPolecatSettings(t *testing.T) {
	tmpDir := t.TempDir()
	// No polecats/ directory anywhere under tmpDir, so findPolecatSettings
	// finds nothing to test against — this must short-circuit before ever
	// spawning claude.
	check := NewHooksLiveFireCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	if result.Status == StatusOK {
		t.Fatalf("must never report StatusOK when no target settings.json exists, got: %s", result.Message)
	}
	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning (inconclusive) for missing target, got %v: %s", result.Status, result.Message)
	}
}

// TestFindPolecatSettings_None verifies findPolecatSettings returns an error
// (not a zero-value path treated as valid) when no polecat settings.json
// exists in the workspace.
func TestFindPolecatSettings_None(t *testing.T) {
	tmpDir := t.TempDir()
	_, _, err := findPolecatSettings(tmpDir)
	if err == nil {
		t.Fatal("expected an error when no polecat settings.json exists")
	}
}
