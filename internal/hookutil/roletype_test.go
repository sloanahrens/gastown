package hookutil

import "testing"

func TestIsAutonomousRole(t *testing.T) {
	t.Parallel()
	autonomous := []string{"polecat", "witness", "deacon", "boot"}
	for _, role := range autonomous {
		if !IsAutonomousRole(role) {
			t.Errorf("IsAutonomousRole(%q) = false, want true", role)
		}
	}

	// refinery (gt-v4ssj.6) and dog (gt-ckunw) roles removed.
	interactive := []string{"mayor", "crew", "refinery", "dog", "unknown", ""}
	for _, role := range interactive {
		if IsAutonomousRole(role) {
			t.Errorf("IsAutonomousRole(%q) = true, want false", role)
		}
	}
}
