package doctor

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/deps"
)

func TestDoltBinaryCheck_Metadata(t *testing.T) {
	t.Parallel()
	check := NewDoltBinaryCheck()

	if check.Name() != "dolt-binary" {
		t.Errorf("Name() = %q, want %q", check.Name(), "dolt-binary")
	}
	if check.Description() != "Check that dolt is installed and meets minimum version" {
		t.Errorf("Description() = %q", check.Description())
	}
	if check.Category() != CategoryInfrastructure {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryInfrastructure)
	}
	if check.CanFix() {
		t.Error("CanFix() should return false (user must install dolt manually)")
	}
}

// The dolt version is parsed and classified in deps (dolt_test.go); the
// check maps each classification to a result.
func TestDoltBinaryCheck_Statuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    deps.DoltStatus
		version   string
		want      CheckStatus
		inMessage string
		inFixHint string
	}{
		{"current", deps.DoltOK, deps.MinDoltVersion, StatusOK, deps.MinDoltVersion, ""},
		{"not in PATH", deps.DoltNotFound, "", StatusError, "dolt not found in PATH", "dolthub/dolt"},
		{"too old", deps.DoltTooOld, "1.0.0", StatusError, "too old", "dolthub/dolt"},
		{"version failed", deps.DoltExecFailed, "", StatusError, "failed", "dolthub/dolt"},
		{"unparseable", deps.DoltUnknown, "", StatusWarning, "could not be parsed", "dolthub/dolt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check := NewDoltBinaryCheck()
			check.probe = func() (deps.DoltStatus, string, string) { return tc.status, tc.version, "detail" }
			result := check.Run(&CheckContext{TownRoot: t.TempDir()})
			if result.Status != tc.want {
				t.Errorf("Status = %v, want %v: %s", result.Status, tc.want, result.Message)
			}
			if !strings.Contains(result.Message, tc.inMessage) {
				t.Errorf("Message %q lacks %q", result.Message, tc.inMessage)
			}
			if !strings.Contains(result.FixHint, tc.inFixHint) {
				t.Errorf("FixHint %q lacks %q", result.FixHint, tc.inFixHint)
			}
		})
	}
}
