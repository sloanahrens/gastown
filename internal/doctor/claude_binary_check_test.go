package doctor

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/deps"
)

func TestClaudeBinaryCheck_Metadata(t *testing.T) {
	t.Parallel()
	check := NewClaudeBinaryCheck()

	if check.Name() != "claude-binary" {
		t.Errorf("Name() = %q, want %q", check.Name(), "claude-binary")
	}
	if check.Description() != "Check that Claude Code meets minimum version for Gas Town" {
		t.Errorf("Description() = %q", check.Description())
	}
	if check.Category() != CategoryInfrastructure {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryInfrastructure)
	}
	if check.CanFix() {
		t.Error("CanFix() should return false")
	}
}

// The claude version is parsed and classified in deps (claude_test.go);
// the check maps each classification to a result.
func TestClaudeBinaryCheck_Statuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    deps.ClaudeCodeStatus
		version   string
		want      CheckStatus
		inMessage []string
		fixHint   bool
	}{
		{"current", deps.ClaudeCodeOK, deps.RecommendedClaudeCodeVersion, StatusOK, []string{deps.RecommendedClaudeCodeVersion}, false},
		{"not in PATH is optional", deps.ClaudeCodeNotFound, "", StatusOK, []string{"not found"}, false},
		{"too old", deps.ClaudeCodeTooOld, "1.0.62", StatusWarning, []string{"below minimum", "1.0.62"}, true},
		{"old but ok", deps.ClaudeCodeOldButOK, "2.0.25", StatusOK, []string{"upgrade", "recommended"}, false},
		{"--version failed", deps.ClaudeCodeExecFailed, "", StatusWarning, []string{"failed"}, true},
		{"unparseable", deps.ClaudeCodeUnknown, "", StatusWarning, []string{"could not be parsed"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check := NewClaudeBinaryCheck()
			check.probe = func() (deps.ClaudeCodeStatus, string) { return tc.status, tc.version }
			result := check.Run(&CheckContext{TownRoot: t.TempDir()})
			if result.Status != tc.want {
				t.Errorf("Status = %v, want %v: %s", result.Status, tc.want, result.Message)
			}
			for _, w := range tc.inMessage {
				if !strings.Contains(result.Message, w) {
					t.Errorf("Message %q lacks %q", result.Message, w)
				}
			}
			if tc.fixHint && result.FixHint == "" {
				t.Error("expected a fix hint")
			}
		})
	}
}
