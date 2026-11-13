package beads

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadDoltMetadataWithoutThePortKey verifies the beads database metadata
// takes its port from dolt-server.port and never from the deprecated
// metadata.json key, while the host still comes from metadata.json (gt-49f3pn).
func TestReadDoltMetadataWithoutThePortKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata string
		portFile string // empty: no dolt-server.port
		wantPort string
	}{
		{
			name:     "port file names the port",
			metadata: `{"dolt_server_host":"10.0.0.5"}`,
			portFile: "4411",
			wantPort: "4411",
		},
		{
			name:     "a leftover key does not set the port",
			metadata: `{"dolt_server_host":"10.0.0.5","dolt_server_port":4407}`,
			wantPort: "",
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
				writePortFileFixture(t, beadsDir, tt.portFile)
			}

			meta := readDoltMetadata(beadsDir)
			if meta.Port != tt.wantPort {
				t.Errorf("Port = %q, want %q", meta.Port, tt.wantPort)
			}
			if meta.Host != "10.0.0.5" {
				t.Errorf("Host = %q, want 10.0.0.5", meta.Host)
			}
		})
	}
}
