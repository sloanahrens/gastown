package doctor

import (
	"errors"
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
// test against: it must report StatusSkipped ("unknown: ..."), never
// StatusOK. This is the same requirement the bead (gt-5ihs) named
// explicitly — earlier verification methods gave a false pass; this check
// must fail toward "could not determine", never toward "pass", when it
// can't actually exercise the real dispatch path.
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
	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped (unknown) for missing target, got %v: %s", result.Status, result.Message)
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

// TestEvaluateLiveFireResult pins the three-way verdict (finding 1 & 2,
// gt-wisp-db27): a created branch is always StatusError regardless of what
// claude's output said; an unblocked-looking run (no branch, but also no
// confirmed block banner) must be inconclusive rather than a false pass;
// only a no-branch run WITH a confirmed block banner earns StatusOK.
func TestEvaluateLiveFireResult(t *testing.T) {
	check := NewHooksLiveFireCheck()

	tests := []struct {
		name           string
		branchCreated  bool
		blockConfirmed bool
		runErr         error
		wantStatus     CheckStatus
	}{
		{
			name:          "branch created — guard failed open",
			branchCreated: true,
			wantStatus:    StatusError,
		},
		{
			name:          "branch created even with a confirmed-looking banner — still an error",
			branchCreated: true, blockConfirmed: true,
			wantStatus: StatusError,
		},
		{
			name:           "no branch, block banner confirmed — real pass",
			branchCreated:  false,
			blockConfirmed: true,
			wantStatus:     StatusOK,
		},
		{
			name:           "no branch, no block banner, clean exit — inconclusive, not a pass",
			branchCreated:  false,
			blockConfirmed: false,
			wantStatus:     StatusSkipped,
		},
		{
			name:           "no branch, no block banner, claude errored — inconclusive (finding 2)",
			branchCreated:  false,
			blockConfirmed: false,
			runErr:         errors.New("exit status 1"),
			wantStatus:     StatusSkipped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := check.evaluateLiveFireResult("test-label", "/fake/settings.json", tt.branchCreated, tt.blockConfirmed, tt.runErr, "stdout text", "stderr text")
			if result.Status != tt.wantStatus {
				t.Errorf("evaluateLiveFireResult() status = %v, want %v (message: %s)", result.Status, tt.wantStatus, result.Message)
			}
			if result.Status == StatusOK && result.Message == "" {
				t.Error("StatusOK result should carry an explanatory message")
			}
		})
	}
}
