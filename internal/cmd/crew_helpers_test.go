package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

func TestCrewSessionName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rigName  string
		crewName string
	}{
		{"basic", "gastown", "tester"},
		{"beads rig", "beads", "emma"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crewSessionName(tt.rigName, tt.crewName)
			if got == "" {
				t.Error("crewSessionName returned empty string")
			}
			// Session name should contain the crew name
			if !containsStr(got, tt.crewName) {
				t.Errorf("crewSessionName(%q, %q) = %q, does not contain crew name", tt.rigName, tt.crewName, got)
			}
		})
	}
}

func TestParseCrewSessionName(t *testing.T) {
	t.Parallel()
	rigName := "gastown"
	crewName := "tester"
	sessionName := session.CrewSessionName("gt", crewName)

	parsedRig, parsedCrew, _, ok := parseCrewSessionName(nudgeTestRegistry(), sessionName)
	if !ok {
		t.Fatalf("parseCrewSessionName(%q) returned ok=false", sessionName)
	}
	if parsedRig != rigName {
		t.Errorf("parsed rig = %q, want %q", parsedRig, rigName)
	}
	if parsedCrew != crewName {
		t.Errorf("parsed crew = %q, want %q", parsedCrew, crewName)
	}
}

func TestParseCrewSessionName_Invalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		session string
	}{
		{"empty", ""},
		{"random", "random-session"},
		{"no crew marker", "gt-gastown-polecat-alpha"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, ok := parseCrewSessionName(nudgeTestRegistry(), tt.session)
			if ok {
				t.Errorf("parseCrewSessionName(%q) returned ok=true, want false", tt.session)
			}
		})
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || indexStr(s, substr) >= 0)
}

func indexStr(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
