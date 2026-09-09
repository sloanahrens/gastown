package doctor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestAgentBeadsShadowCheck_ReportsTownDuplicates(t *testing.T) {
	tmpDir := setupTownDuplicateFixture(t)
	writeTownDuplicateBdScript(t, tmpDir, filepath.Join(tmpDir, "bd.log"))

	check := NewAgentBeadsShadowCheck()
	result := check.Run(&CheckContext{TownRoot: tmpDir})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; message: %s", result.Status, result.Message)
	}
	for _, want := range []string{"gs-gastown-witness", "gs-gastown-refinery"} {
		found := false
		for _, d := range result.Details {
			if strings.HasPrefix(d, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %s in details, got %v", want, result.Details)
		}
	}
	if !strings.Contains(result.FixHint, "gt polecat identity reconcile") {
		t.Errorf("fix hint must name the reconcile command, got %q", result.FixHint)
	}
	if check.CanFix() {
		t.Error("shadow check must never auto-fix")
	}
}

func TestAgentBeadsShadowCheck_CleanWhenNoRoutes(t *testing.T) {
	result := NewAgentBeadsShadowCheck().Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK (no routes => nothing to shadow)", result.Status)
	}
}

func TestShadowedAgentFields(t *testing.T) {
	rig := &beads.Issue{Description: "x\n\nrole_type: polecat\nagent_state: done\nactive_mr: gt-wisp-0yhh\n"}
	town := &beads.Issue{Description: "x\n\nrole_type: polecat\nagent_state: done\nactive_mr: null\n"}
	got := shadowedAgentFields(rig, town)
	if len(got) != 1 || got[0] != "active_mr" {
		t.Fatalf("shadowedAgentFields = %v, want [active_mr]", got)
	}
}
