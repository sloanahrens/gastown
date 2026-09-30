package hookutil

import "testing"

func TestIsAutonomousRole(t *testing.T) {
	t.Parallel()
	autonomous := []string{"polecat"}
	for _, role := range autonomous {
		if !IsAutonomousRole(role) {
			t.Errorf("IsAutonomousRole(%q) = false, want true", role)
		}
	}

	// refinery (gt-v4ssj.6), dog (gt-ckunw) and witness, deacon and boot
	// (gt-4k3fj.6.1) roles removed.
	interactive := []string{"mayor", "crew", "refinery", "dog", "witness", "deacon", "boot", "unknown", ""}
	for _, role := range interactive {
		if IsAutonomousRole(role) {
			t.Errorf("IsAutonomousRole(%q) = true, want false", role)
		}
	}
}
