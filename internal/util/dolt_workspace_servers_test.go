//go:build !windows

package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBeadsDirFromConfigPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		configPath    string
		wantBeadsDir  string
		wantWorkspace string
		wantOK        bool
	}{
		{
			name:          "bd auto-start config",
			configPath:    "/home/me/.claude/.beads/dolt-server-config.yaml",
			wantBeadsDir:  "/home/me/.claude/.beads",
			wantWorkspace: "/home/me/.claude",
			wantOK:        true,
		},
		{
			name:          "config nested under the beads dir",
			configPath:    "/home/me/proj/.beads/dolt/config.yaml",
			wantBeadsDir:  "/home/me/proj/.beads",
			wantWorkspace: "/home/me/proj",
			wantOK:        true,
		},
		{
			name:       "the town's own server",
			configPath: "/Users/sloan/gt/.dolt-data/config.yaml",
			wantOK:     false,
		},
		{
			name:       "no config at all",
			configPath: "",
			wantOK:     false,
		},
		{
			name:       "a bare .beads dir is not a config path",
			configPath: ".beads",
			wantOK:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			beadsDir, workspace, ok := BeadsDirFromConfigPath(tt.configPath)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if beadsDir != tt.wantBeadsDir {
				t.Errorf("beadsDir = %q, want %q", beadsDir, tt.wantBeadsDir)
			}
			if workspace != tt.wantWorkspace {
				t.Errorf("workspace = %q, want %q", workspace, tt.wantWorkspace)
			}
		})
	}
}

// TestDoltConfigListenerPort pins the port an imposter's config names: bd
// starts the server with --config and no --port, so the random port exists
// only in the file (gt-4js4f).
func TestDoltConfigListenerPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want int
	}{
		{"bd workspace config", "log_level: warning\nlistener:\n    host: 127.0.0.1\n    port: 61260\ncfg_dir: /x/.doltcfg\n", 61260},
		{"town config", "log_level: warning\n\nlistener:\n  port: 3307\n  max_connections: 1000\ndata_dir: \"/x/.dolt-data\"\n", 3307},
		{"no listener block", "log_level: warning\n", 0},
		{"no port in the listener block", "listener:\n  host: 127.0.0.1\n", 0},
		{"commented port", "listener:\n  # port: 3307\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "dolt-server-config.yaml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := doltConfigListenerPort(path); got != tt.want {
				t.Errorf("doltConfigListenerPort() = %d, want %d", got, tt.want)
			}
		})
	}
	if got := doltConfigListenerPort(filepath.Join(t.TempDir(), "missing.yaml")); got != 0 {
		t.Errorf("doltConfigListenerPort(missing) = %d, want 0", got)
	}
}
