package doctor

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// testRegistryForNameCheck returns a PrefixRegistry with a few known rigs
// suitable for session-name-format tests.
func testRegistryForNameCheck() *session.PrefixRegistry {
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("nif", "niflheim")
	reg.Register("wa", "whatsapp_automation")
	return reg
}

func TestNewMalformedSessionNameCheck(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()

	if check.Name() != "session-name-format" {
		t.Errorf("expected name 'session-name-format', got %q", check.Name())
	}

	if check.Description() != "Detect sessions with outdated Gas Town naming format" {
		t.Errorf("unexpected description: %q", check.Description())
	}

	if check.CanFix() {
		t.Error("expected CanFix to return false (crew sessions are renamed manually)")
	}

	if check.Category() != CategoryCleanup {
		t.Errorf("expected category %q, got %q", CategoryCleanup, check.Category())
	}
}

func TestMalformedSessionNameCheck_Run_NoSessions(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()
	check.sessionListerForTest = &mockSessionLister{sessions: []string{}}
	check.registryForTest = testRegistryForNameCheck()

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK with no sessions, got %v: %s", result.Status, result.Message)
	}
}

// TestMalformedSessionNameCheck_Run_AllCorrect verifies that sessions already
// in canonical format produce a clean result. This test uses a populated
// registry so sessions actually parse — it does not pass vacuously.
func TestMalformedSessionNameCheck_Run_AllCorrect(t *testing.T) {
	t.Parallel()
	reg := testRegistryForNameCheck()
	check := NewMalformedSessionNameCheck()
	check.registryForTest = reg
	check.sessionListerForTest = &mockSessionLister{sessions: []string{
		"hq-mayor",
		"gt-crew-max",
		"nif-crew-wolf",
		"wa-crew-batista",
	}}

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK for correctly-named sessions, got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}
}

func TestMalformedSessionNameCheck_Run_NonGasTownSessions(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()
	check.registryForTest = testRegistryForNameCheck()
	check.sessionListerForTest = &mockSessionLister{sessions: []string{
		"my-personal-session",
		"vim",
		"jupyter",
	}}

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK for non-Gas Town sessions, got %v", result.Status)
	}
}

// TestMalformedSessionNameCheck_Run_NonGasTownWithRigSubstring verifies that
// non-Gastown sessions whose names happen to contain a rig name are NOT
// falsely flagged. The ownership guard requires a known Gastown prefix.
func TestMalformedSessionNameCheck_Run_NonGasTownWithRigSubstring(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()
	check.registryForTest = testRegistryForNameCheck()
	check.sessionListerForTest = &mockSessionLister{sessions: []string{
		"my-niflheim-crew-max",              // "my" is not a known Gastown prefix
		"foo-gastown-crew-max",              // "foo" is not a known Gastown prefix
		"test-whatsapp_automation-crew-max", // "test" is not a known Gastown prefix
	}}

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK for non-Gastown sessions with rig substrings, got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}
}

// TestMalformedSessionNameCheck_Run_PolecatWithRigSubstring verifies that
// polecat sessions whose names embed a rig name are NOT falsely flagged.
// E.g., "gt-fix-gastown-crew-max" is a polecat named "fix-gastown-crew-max",
// not a legacy gastown crew session.
func TestMalformedSessionNameCheck_Run_PolecatWithRigSubstring(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()
	check.registryForTest = testRegistryForNameCheck()
	check.sessionListerForTest = &mockSessionLister{sessions: []string{
		"gt-fix-gastown-crew-max",     // polecat "fix-gastown-crew-max", prefix "gt-fix" is not known
		"nif-debug-niflheim-crew-max", // prefix "nif-debug" is not a known prefix
	}}

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK for polecat sessions with rig substrings, got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}
}

// TestMalformedSessionNameCheck_Run_DetectsMismatch is the core test.
// It verifies that a genuine legacy name (gt-niflheim-crew-wolf) is detected
// and the canonical name (nif-crew-wolf) is reported.
func TestMalformedSessionNameCheck_Run_DetectsMismatch(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()
	check.registryForTest = testRegistryForNameCheck()
	check.sessionListerForTest = &mockSessionLister{sessions: []string{
		"hq-mayor",
		"gt-niflheim-crew-wolf", // legacy: should be nif-crew-wolf
		"gt-niflheim-crew-bear", // legacy: should be nif-crew-bear
		"nif-crew-bear",         // already canonical — should not be flagged
	}}

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected Warning for legacy sessions, got %v: %s", result.Status, result.Message)
	}

	// Should find exactly the 2 legacy sessions.
	if len(result.Details) != 2 {
		t.Errorf("expected 2 details, got %d: %v", len(result.Details), result.Details)
	}

	// Verify the canonical renames are present in the details.
	for _, want := range []string{"gt-niflheim-crew-wolf", "nif-crew-wolf", "gt-niflheim-crew-bear", "nif-crew-bear"} {
		found := false
		for _, d := range result.Details {
			if strings.Contains(d, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected details to contain %q, got: %v", want, result.Details)
		}
	}
}

// TestMalformedSessionNameCheck_Run_LegacyWACrew verifies the stated use
// case: gt-whatsapp_automation-crew-max → wa-crew-max.
func TestMalformedSessionNameCheck_Run_LegacyWACrew(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()
	check.registryForTest = testRegistryForNameCheck()
	check.sessionListerForTest = &mockSessionLister{sessions: []string{
		"gt-whatsapp_automation-crew-max",
	}}

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Fatalf("expected Warning for legacy wa crew session, got %v", result.Status)
	}
	if len(result.Details) != 1 {
		t.Fatalf("expected 1 detail, got %d: %v", len(result.Details), result.Details)
	}
	d := result.Details[0]
	if !strings.Contains(d, "gt-whatsapp_automation-crew-max") || !strings.Contains(d, "wa-crew-max") {
		t.Errorf("expected detail to map legacy → canonical, got: %q", d)
	}
}

// TestMalformedSessionNameCheck_Run_CrewSession verifies that legacy crew
// sessions are detected and flagged with a "manual rename required" note,
// since Fix() cannot safely rename attached crew sessions.
func TestMalformedSessionNameCheck_Run_CrewSession(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()
	check.registryForTest = testRegistryForNameCheck()
	check.sessionListerForTest = &mockSessionLister{sessions: []string{
		"gt-niflheim-crew-wolf",
	}}

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Fatalf("expected Warning for legacy crew session, got %v", result.Status)
	}
	// Detail must mention "manual rename" so the user knows --fix won't fix it.
	for _, d := range result.Details {
		if strings.Contains(d, "gt-niflheim-crew-wolf") {
			if !strings.Contains(d, "manual") {
				t.Errorf("crew session detail should mention manual rename, got: %q", d)
			}
			return
		}
	}
	t.Errorf("crew session not found in details: %v", result.Details)
}

// TestMalformedSessionNameCheck_Run_RetiredRoleSuffixIgnored verifies that
// leftover sessions named for retired roles (witness, refinery) are not
// reported: nothing renames or restarts them any more.
func TestMalformedSessionNameCheck_Run_RetiredRoleSuffixIgnored(t *testing.T) {
	t.Parallel()
	check := NewMalformedSessionNameCheck()
	check.registryForTest = testRegistryForNameCheck()
	check.sessionListerForTest = &mockSessionLister{sessions: []string{
		"gt-niflheim-witness",
		"gt-niflheim-refinery",
	}}

	ctx := &CheckContext{TownRoot: t.TempDir()}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected OK for retired-role sessions, got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}
}
