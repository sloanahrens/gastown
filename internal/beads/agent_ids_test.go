package beads

import (
	"testing"
)

// TestMayorBeadIDTown tests the town-level Mayor bead ID.
func TestMayorBeadIDTown(t *testing.T) {
	got := MayorBeadIDTown()
	want := "hq-mayor"
	if got != want {
		t.Errorf("MayorBeadIDTown() = %q, want %q", got, want)
	}
}

// TestDogBeadIDTown tests town-level Dog bead IDs.
func TestDogBeadIDTown(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"alpha", "hq-dog-alpha"},
		{"rex", "hq-dog-rex"},
		{"spot", "hq-dog-spot"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DogBeadIDTown(tt.name)
			if got != tt.want {
				t.Errorf("DogBeadIDTown(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestAgentBeadIDWithPrefix tests agent bead ID generation, including dedup when prefix == rig.
func TestAgentBeadIDWithPrefix(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		rig    string
		role   string
		wname  string
		want   string
	}{
		// Normal cases (prefix != rig)
		{"town-level mayor", "gt", "", "mayor", "", "gt-mayor"},
		{"rig witness", "gt", "gastown", "witness", "", "gt-gastown-witness"},
		{"rig polecat", "gt", "gastown", "polecat", "nux", "gt-gastown-polecat-nux"},
		{"rig crew", "bd", "beads", "crew", "dave", "bd-beads-crew-dave"},

		// Collapsed cases (prefix == rig) — should NOT stutter
		{"dedup witness", "ff", "ff", "witness", "", "ff-witness"},
		{"dedup refinery", "ff", "ff", "refinery", "", "ff-refinery"},
		{"dedup polecat", "ff", "ff", "polecat", "nux", "ff-polecat-nux"},
		{"dedup crew", "ff", "ff", "crew", "dave", "ff-crew-dave"},
		{"dedup bd-beads", "bd", "bd", "witness", "", "bd-witness"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AgentBeadIDWithPrefix(tt.prefix, tt.rig, tt.role, tt.wname)
			if got != tt.want {
				t.Errorf("AgentBeadIDWithPrefix(%q, %q, %q, %q) = %q, want %q",
					tt.prefix, tt.rig, tt.role, tt.wname, got, tt.want)
			}
		})
	}
}

// TestExtractAgentPrefix tests prefix extraction from agent IDs.
func TestExtractAgentPrefix(t *testing.T) {
	tests := []struct {
		name       string
		id         string
		wantPrefix string
	}{
		// Town-level agents
		{"mayor", "gt-mayor", "gt"},
		{"deacon", "gt-deacon", "gt"},
		{"bd mayor", "bd-mayor", "bd"},

		// Town-level named (dogs)
		{"dog", "gt-dog-alpha", "gt"},
		{"dog hyphen name", "gt-dog-war-boy", "gt"},

		// Per-rig agents
		{"witness", "gt-gastown-witness", "gt"},
		{"refinery", "bd-beads-refinery", "bd"},

		// Named agents - the bug case
		{"polecat 3-char name", "nx-nexus-polecat-nux", "nx"},
		{"polecat regular", "gt-gastown-polecat-phoenix", "gt"},
		{"crew", "gt-beads-crew-dave", "gt"},

		// Hyphenated rig names
		{"hyphenated rig", "gt-my-project-witness", "gt"},
		{"multi-hyphen rig polecat", "bd-my-cool-app-polecat-bob", "bd"},

		// Edge cases
		{"no hyphen", "nohyphen", ""},
		{"empty", "", ""},
		{"just prefix", "gt-", "gt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractAgentPrefix(tt.id)
			if got != tt.wantPrefix {
				t.Errorf("ExtractAgentPrefix(%q) = %q, want %q", tt.id, got, tt.wantPrefix)
			}
		})
	}
}
