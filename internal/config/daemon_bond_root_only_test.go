package config

import (
	"os"
	"path/filepath"
	"testing"
)

// bond_root_only is the operator key for the root-only bond: it loads from the
// daemon settings file, and an absent key reads false.
func TestBondRootOnlyConfigKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"absent", "", false},
		{"false", `,"bond_root_only":false`, false},
		{"true", `,"bond_root_only":true`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.json")
			raw := `{"type":"daemon-patrol-config","version":1` + tc.key + `}`
			if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}

			cfg, err := LoadDaemonPatrolConfig(path)
			if err != nil {
				t.Fatalf("LoadDaemonPatrolConfig: %v", err)
			}
			if cfg.BondRootOnly != tc.want {
				t.Errorf("BondRootOnly = %v, want %v", cfg.BondRootOnly, tc.want)
			}
		})
	}
}
