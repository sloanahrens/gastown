package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCrewPristineScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rigFlag  string
		args     []string
		wantRig  string
		wantName string
	}{
		{"no args", "", nil, "", ""},
		{"no args with --rig", "beads", nil, "beads", ""},
		{"bare name", "", []string{"emma"}, "", "emma"},
		{"bare name with --rig", "beads", []string{"emma"}, "beads", "emma"},
		{"rig/name", "", []string{"beads/emma"}, "beads", "emma"},
		{"--rig wins over the prefix", "gastown", []string{"beads/emma"}, "gastown", "emma"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRig, gotName := crewPristineScope(tt.rigFlag, tt.args)
			if gotRig != tt.wantRig || gotName != tt.wantName {
				t.Errorf("crewPristineScope(%q, %v) = (%q, %q), want (%q, %q)",
					tt.rigFlag, tt.args, gotRig, gotName, tt.wantRig, tt.wantName)
			}
		})
	}
}

// TestCrewPristineResolvesPrefixRig: rig/name acts on that rig's member from
// inside another rig and from the town root; a bare name stays in the cwd rig.
func TestCrewPristineResolvesPrefixRig(t *testing.T) {
	root := crewStopTown(t)
	for _, rigName := range []string{"beads", "gastown"} {
		if err := os.MkdirAll(filepath.Join(root, rigName, "crew", "emma"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name    string
		cwd     string
		arg     string
		wantRig string
	}{
		{"prefix from another rig", filepath.Join(root, "gastown"), "beads/emma", "beads"},
		{"prefix from the town root", root, "beads/emma", "beads"},
		{"bare name in the cwd rig", filepath.Join(root, "gastown"), "emma", "gastown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.cwd)
			rigName, name := crewPristineScope("", []string{tt.arg})
			mgr, r, err := getCrewManager(rigName)
			if err != nil {
				t.Fatalf("getCrewManager(%q): %v", rigName, err)
			}
			w, err := mgr.Get(name)
			if err != nil {
				t.Fatalf("Get(%q): %v", name, err)
			}
			if r.Name != tt.wantRig || w.Rig != tt.wantRig {
				t.Errorf("gt crew pristine %s from %s acted on rig %q (worker rig %q), want %q",
					tt.arg, tt.cwd, r.Name, w.Rig, tt.wantRig)
			}
		})
	}
}
