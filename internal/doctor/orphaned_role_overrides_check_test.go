package doctor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOrphanedRoleOverridesCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		townFiles  []string // relative to town root
		rigFiles   map[string][]string
		bareFiles  []string // relative to town root, in a directory that is not a rig
		wantStatus CheckStatus
		wantDetail []string // relative to town root
	}{
		{
			name:       "no role directories",
			wantStatus: StatusOK,
		},
		{
			name:       "town-level override is reported",
			townFiles:  []string{"roles/polecat.toml"},
			wantStatus: StatusWarning,
			wantDetail: []string{"roles/polecat.toml"},
		},
		{
			name:       "rig-level override is reported",
			rigFiles:   map[string][]string{"gastown": {"roles/witness.toml"}},
			wantStatus: StatusWarning,
			wantDetail: []string{"gastown/roles/witness.toml"},
		},
		{
			name:       "non-toml files are ignored",
			townFiles:  []string{"roles/notes.md"},
			rigFiles:   map[string][]string{"gastown": {"roles/README"}},
			wantStatus: StatusOK,
		},
		{
			name:       "roles directory in a directory without rig.json is not scanned",
			bareFiles:  []string{"scratch/roles/polecat.toml"},
			wantStatus: StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()

			for _, f := range tt.townFiles {
				writeTestFile(t, filepath.Join(townRoot, f))
			}
			for _, f := range tt.bareFiles {
				writeTestFile(t, filepath.Join(townRoot, f))
			}
			for rig, files := range tt.rigFiles {
				writeTestFile(t, filepath.Join(townRoot, rig, "rig.json"))
				for _, f := range files {
					writeTestFile(t, filepath.Join(townRoot, rig, f))
				}
			}

			result := NewOrphanedRoleOverridesCheck().Run(&CheckContext{TownRoot: townRoot})

			if result.Status != tt.wantStatus {
				t.Fatalf("status = %v, want %v (message %q)", result.Status, tt.wantStatus, result.Message)
			}
			var want []string
			for _, d := range tt.wantDetail {
				want = append(want, filepath.Join(townRoot, d))
			}
			if !slices.Equal(result.Details, want) {
				t.Errorf("details = %v, want %v", result.Details, want)
			}
		})
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
}
