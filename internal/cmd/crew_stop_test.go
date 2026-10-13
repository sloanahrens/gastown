package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const crewStopRigsJSON = `{"version":1,"rigs":{` +
	`"beads":{"git_url":"https://example.com/beads.git","added_at":"2026-01-01T00:00:00Z"},` +
	`"gastown":{"git_url":"https://example.com/gastown.git","added_at":"2026-01-01T00:00:00Z"}}}`

func crewStopTown(t *testing.T) string {
	t.Helper()
	root := makeTestTownRoot(t)
	if err := os.WriteFile(filepath.Join(root, "mayor", "rigs.json"), []byte(crewStopRigsJSON), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

func crewStopSessions() []*AgentSession {
	return []*AgentSession{
		{Name: "bd-crew-x", Type: AgentCrew, Rig: "beads", AgentName: "x"},
		{Name: "bd-crew-y", Type: AgentCrew, Rig: "beads", AgentName: "y"},
		{Name: "gt-crew-z", Type: AgentCrew, Rig: "gastown", AgentName: "z"},
		{Name: "gt-nux", Type: AgentPolecat, Rig: "beads", AgentName: "nux"},
	}
}

func sessionNames(agents []*AgentSession) string {
	var names []string
	for _, a := range agents {
		names = append(names, a.Name)
	}
	return strings.Join(names, ",")
}

func TestResolveCrewStopRig(t *testing.T) {
	t.Parallel()
	root := crewStopTown(t)

	tests := []struct {
		name    string
		dir     string
		want    string
		wantErr bool
	}{
		{"crew dir of a rig", filepath.Join(root, "beads", "crew", "x"), "beads", false},
		{"rig root", filepath.Join(root, "gastown"), "gastown", false},
		{"town root", root, "", true},
		{"mayor dir is not a rig", filepath.Join(root, "mayor"), "", true},
		{"unregistered dir", filepath.Join(root, "scratch", "sub"), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveCrewStopRig(root, tt.dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveCrewStopRig(%q) = %q, want error", tt.dir, got)
				}
				if !strings.Contains(err.Error(), "--rig") {
					t.Errorf("error %q does not name --rig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCrewStopRig(%q): %v", tt.dir, err)
			}
			if got != tt.want {
				t.Errorf("resolveCrewStopRig(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestCrewStopTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rig  string
		want string
	}{
		{"cwd rig beads", "beads", "bd-crew-x,bd-crew-y"},
		{"--rig gastown", "gastown", "gt-crew-z"},
		{"no rig (--all) keeps every rig", "", "bd-crew-x,bd-crew-y,gt-crew-z"},
		{"rig with no crew", "other", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionNames(crewStopTargets(crewStopSessions(), tt.rig)); got != tt.want {
				t.Errorf("crewStopTargets(rig=%q) = %q, want %q", tt.rig, got, tt.want)
			}
		})
	}
}

// A bare `gt crew stop` from inside a rig's crew dir narrows to that rig.
func TestCrewStopNoArgsFromRigStopsOnlyThatRig(t *testing.T) {
	t.Parallel()
	root := crewStopTown(t)

	rig, err := resolveCrewStopRig(root, filepath.Join(root, "beads", "crew", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sessionNames(crewStopTargets(crewStopSessions(), rig)), "bd-crew-x,bd-crew-y"; got != want {
		t.Errorf("targets = %q, want %q", got, want)
	}
}

// A bare `gt crew stop` outside any rig errors, so runCrewStop returns before
// it lists a session and nothing is stopped.
func TestCrewStopNoArgsOutsideRigRefuses(t *testing.T) {
	t.Parallel()
	root := crewStopTown(t)

	rig, err := resolveCrewStopRig(root, filepath.Join(root, "mayor"))
	if err == nil {
		t.Fatalf("resolveCrewStopRig outside a rig = %q, want error", rig)
	}
	if rig != "" {
		t.Errorf("rig = %q on refusal, want empty (an empty rig would stop every rig)", rig)
	}
}
