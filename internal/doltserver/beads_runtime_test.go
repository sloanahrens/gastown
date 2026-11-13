package doltserver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadBeadsRuntimeConfigWithoutThePortKey verifies the port comes from
// dolt-server.port, which wins over the deprecated metadata.json key, and that
// the key is only the fallback while no port file exists (gt-49f3pn).
func TestReadBeadsRuntimeConfigWithoutThePortKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata string
		portFile string // empty: no dolt-server.port
		wantPort int
	}{
		{
			name:     "port file names the port",
			metadata: `{"backend":"dolt","dolt_mode":"server","dolt_server_host":"10.0.0.5"}`,
			portFile: "4411",
			wantPort: 4411,
		},
		{
			name:     "no port file falls back to the default port",
			metadata: `{"backend":"dolt","dolt_mode":"server","dolt_server_host":"10.0.0.5"}`,
			wantPort: DefaultPort,
		},
		{
			name:     "key and a different port file: the file wins",
			metadata: `{"backend":"dolt","dolt_mode":"server","dolt_server_port":4407}`,
			portFile: "4411",
			wantPort: 4411,
		},
		{
			name:     "key only: the key is the fallback",
			metadata: `{"backend":"dolt","dolt_mode":"server","dolt_server_port":4407}`,
			wantPort: 4407,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			beadsDir := filepath.Join(t.TempDir(), ".beads")
			if err := os.MkdirAll(beadsDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(tt.metadata), 0644); err != nil {
				t.Fatal(err)
			}
			if tt.portFile != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(tt.portFile), 0644); err != nil {
					t.Fatal(err)
				}
			}

			cfg, ok := ReadBeadsRuntimeConfig(beadsDir)
			if !ok {
				t.Fatal("ReadBeadsRuntimeConfig ok = false, want a server-mode store")
			}
			if cfg.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", cfg.Port, tt.wantPort)
			}
		})
	}
}
