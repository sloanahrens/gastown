package doctor

import (
	"runtime"
	"testing"
)

func TestNewDoltOrphanServersCheck(t *testing.T) {
	check := NewDoltOrphanServersCheck()

	if check.Name() != "dolt-orphan-servers" {
		t.Errorf("expected name 'dolt-orphan-servers', got %q", check.Name())
	}
	if check.Category() != CategoryCleanup {
		t.Errorf("expected category %q, got %q", CategoryCleanup, check.Category())
	}
	if !check.CanFix() {
		t.Error("expected CanFix to return true — orphaned servers and stale temp dirs are auto-fixable")
	}
}

func TestDoltOrphanServersCheck_Run(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dolt orphan server detection is not supported on Windows")
	}

	// Results depend on the real process table and $TMPDIR on the test
	// machine, so this only verifies the check runs cleanly and reports a
	// status consistent with what it found — mirrors
	// TestOrphanProcessCheck_Run's approach for the analogous claude-process check.
	check := NewDoltOrphanServersCheck()
	ctx := &CheckContext{TownRoot: t.TempDir()}

	result := check.Run(ctx)

	if result.Status != StatusOK && result.Status != StatusWarning {
		t.Errorf("expected StatusOK or StatusWarning, got %v: %s", result.Status, result.Message)
	}
	if result.Status == StatusOK && result.FixHint != "" {
		t.Errorf("expected no FixHint on a clean run, got %q", result.FixHint)
	}
	if result.Status == StatusWarning && result.FixHint == "" {
		t.Error("expected a FixHint when orphans or stale dirs are reported")
	}
}

func TestDoltOrphanServersCheck_FixIsSafeWithNoFindings(t *testing.T) {
	check := NewDoltOrphanServersCheck()
	ctx := &CheckContext{TownRoot: t.TempDir()}

	// Fix must not panic or error when Run hasn't cached any findings.
	if err := check.Fix(ctx); err != nil {
		t.Errorf("Fix() with no cached findings returned error: %v", err)
	}
}
