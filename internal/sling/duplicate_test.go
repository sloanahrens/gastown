package sling

import "testing"

func TestIsPackageFixtureTest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"TestMain", true},
		{"TestMain_", true},
		{"TestMaintenanceWindowExpiry", false},
		{"TestMainFrame", false},
		{"TestHermeticHarnessEnforced", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsPackageFixtureTest(tt.name); got != tt.want {
			t.Errorf("IsPackageFixtureTest(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
