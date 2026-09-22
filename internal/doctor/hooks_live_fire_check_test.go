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

// TestEvaluateBlockedShape pins the three-way verdict (finding 1 & 2,
// gt-wisp-db27): a created branch is always a fail regardless of what
// claude's output said; an unblocked-looking run (no branch, but also no
// confirmed block banner) must be inconclusive rather than a false pass;
// only a no-branch run WITH a confirmed block banner earns a pass.
func TestEvaluateBlockedShape(t *testing.T) {
	tests := []struct {
		name           string
		branchCreated  bool
		blockConfirmed bool
		runErr         error
		wantVerdict    LiveFireVerdict
	}{
		{
			name:          "branch created — guard failed open",
			branchCreated: true,
			wantVerdict:   LiveFireFail,
		},
		{
			name:          "branch created even with a confirmed-looking banner — still a fail",
			branchCreated: true, blockConfirmed: true,
			wantVerdict: LiveFireFail,
		},
		{
			name:           "no branch, block banner confirmed — real pass",
			branchCreated:  false,
			blockConfirmed: true,
			wantVerdict:    LiveFirePass,
		},
		{
			name:           "no branch, no block banner, clean exit — inconclusive, not a pass",
			branchCreated:  false,
			blockConfirmed: false,
			wantVerdict:    LiveFireInconclusive,
		},
		{
			name:           "no branch, no block banner, claude errored — inconclusive (finding 2)",
			branchCreated:  false,
			blockConfirmed: false,
			runErr:         errors.New("exit status 1"),
			wantVerdict:    LiveFireInconclusive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := evaluateBlockedShape("/fake/settings.json", tt.branchCreated, tt.blockConfirmed, tt.runErr, "stdout text", "stderr text")
			if result.Verdict != tt.wantVerdict {
				t.Errorf("evaluateBlockedShape() verdict = %v, want %v (detail: %s)", result.Verdict, tt.wantVerdict, result.Detail)
			}
			if result.Verdict == LiveFirePass && result.Detail == "" {
				t.Error("pass result should carry an explanatory detail")
			}
		})
	}
}

// TestEvaluateAllowedShape covers the mirror-image probe. The case named
// "dropped 'if' field / over-blocking regression" is the regression this
// shape exists to catch: on 2026-09-10 a hooks-sync round-trip dropped the
// pr-workflow block's 'if' scoping field, turning its matcher unconditional,
// so an always-allowed command like 'git status' was blocked too. The
// blocked-only probe could never see this — it only exercises the one
// command that's supposed to be blocked. This shape must fail when the
// marker file never appears but the guard's own block banner did.
func TestEvaluateAllowedShape(t *testing.T) {
	tests := []struct {
		name           string
		markerCreated  bool
		blockConfirmed bool
		runErr         error
		wantVerdict    LiveFireVerdict
	}{
		{
			name:          "marker created — guard correctly allowed the command",
			markerCreated: true,
			wantVerdict:   LiveFirePass,
		},
		{
			name:          "marker created even with a stray banner-like line — still a pass",
			markerCreated: true, blockConfirmed: true,
			wantVerdict: LiveFirePass,
		},
		{
			name:           "dropped 'if' field / over-blocking regression: no marker, block banner confirmed",
			markerCreated:  false,
			blockConfirmed: true,
			wantVerdict:    LiveFireFail,
		},
		{
			name:           "no marker, no block banner, clean exit — inconclusive, not a pass",
			markerCreated:  false,
			blockConfirmed: false,
			wantVerdict:    LiveFireInconclusive,
		},
		{
			name:           "no marker, no block banner, claude errored — inconclusive",
			markerCreated:  false,
			blockConfirmed: false,
			runErr:         errors.New("exit status 1"),
			wantVerdict:    LiveFireInconclusive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := evaluateAllowedShape("/fake/settings.json", tt.markerCreated, tt.blockConfirmed, tt.runErr, "stdout text", "stderr text")
			if result.Verdict != tt.wantVerdict {
				t.Errorf("evaluateAllowedShape() verdict = %v, want %v (detail: %s)", result.Verdict, tt.wantVerdict, result.Detail)
			}
			if result.Verdict == LiveFirePass && result.Detail == "" {
				t.Error("pass result should carry an explanatory detail")
			}
		})
	}
}

// TestEvaluatePairResult pins the combined pair verdict: both shapes must
// pass for StatusOK; a confirmed failure in either shape is StatusError
// naming that shape; any other combination (including one pass, one
// inconclusive) stays StatusSkipped rather than either a false pass or an
// unearned failure.
func TestEvaluatePairResult(t *testing.T) {
	check := NewHooksLiveFireCheck()

	tests := []struct {
		name       string
		blocked    LiveFireShapeResult
		allowed    LiveFireShapeResult
		wantStatus CheckStatus
	}{
		{
			name:       "both pass — real pass",
			blocked:    LiveFireShapeResult{Verdict: LiveFirePass, Detail: "blocked ok"},
			allowed:    LiveFireShapeResult{Verdict: LiveFirePass, Detail: "allowed ok"},
			wantStatus: StatusOK,
		},
		{
			name:       "blocked shape fails — error even if allowed passed",
			blocked:    LiveFireShapeResult{Verdict: LiveFireFail, Detail: "guard failed open"},
			allowed:    LiveFireShapeResult{Verdict: LiveFirePass, Detail: "allowed ok"},
			wantStatus: StatusError,
		},
		{
			name:       "allowed shape fails — error even if blocked passed",
			blocked:    LiveFireShapeResult{Verdict: LiveFirePass, Detail: "blocked ok"},
			allowed:    LiveFireShapeResult{Verdict: LiveFireFail, Detail: "over-blocked"},
			wantStatus: StatusError,
		},
		{
			name:       "one pass, one inconclusive — skipped, not a pass",
			blocked:    LiveFireShapeResult{Verdict: LiveFirePass, Detail: "blocked ok"},
			allowed:    LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: "claude never attempted it"},
			wantStatus: StatusSkipped,
		},
		{
			name:       "both inconclusive — skipped",
			blocked:    LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: "timeout"},
			allowed:    LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: "timeout"},
			wantStatus: StatusSkipped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pair := &LiveFirePairResult{
				Label:        "test-label",
				SettingsPath: "/fake/settings.json",
				Blocked:      tt.blocked,
				Allowed:      tt.allowed,
			}
			result := check.evaluatePairResult(pair)
			if result.Status != tt.wantStatus {
				t.Errorf("evaluatePairResult() status = %v, want %v (message: %s)", result.Status, tt.wantStatus, result.Message)
			}
			if result.Status == StatusOK && result.Message == "" {
				t.Error("StatusOK result should carry an explanatory message")
			}
		})
	}
}
