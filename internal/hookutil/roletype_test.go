package hookutil

import "testing"

func TestIsAutonomousRole(t *testing.T) {
	t.Parallel()
	autonomous := []string{"polecat", "witness", "deacon", "boot", "dog"}
	for _, role := range autonomous {
		if !IsAutonomousRole(role) {
			t.Errorf("IsAutonomousRole(%q) = false, want true", role)
		}
	}

	interactive := []string{"mayor", "crew", "refinery", "unknown", ""} // refinery role removed (gt-v4ssj.6)
	for _, role := range interactive {
		if IsAutonomousRole(role) {
			t.Errorf("IsAutonomousRole(%q) = true, want false", role)
		}
	}
}
