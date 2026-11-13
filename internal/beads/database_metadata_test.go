package beads

import (
	"os"
	"path/filepath"
	"testing"
)

// writeMetadataFixture writes beadsDir/metadata.json with the given content and,
// when portFile is not empty, beadsDir/dolt-server.port.
func writeMetadataFixture(t *testing.T, metadata, portFile string) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	if portFile != "" {
		writePortFileFixture(t, beadsDir, portFile)
	}
	return beadsDir
}

// TestReadDoltMetadataWithoutThePortKey verifies the beads database metadata
// takes its port from dolt-server.port when the deprecated metadata.json key is
// absent, and that the host still comes from metadata.json (gt-49f3pn).
func TestReadDoltMetadataWithoutThePortKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		portFile string // empty: no dolt-server.port
		wantPort string
	}{
		{name: "port file names the port", portFile: "4411", wantPort: "4411"},
		{name: "no port file leaves the port empty", wantPort: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			beadsDir := writeMetadataFixture(t, `{"dolt_server_host":"10.0.0.5"}`, tt.portFile)

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

// TestReadDoltMetadataPortFileBeforeKey verifies the port file is read first and
// the deprecated key is only the fallback while no port file exists (gt-49f3pn).
func TestReadDoltMetadataPortFileBeforeKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		portFile string // empty: no dolt-server.port
		wantPort string
	}{
		{name: "key and a different port file: the file wins", portFile: "4411", wantPort: "4411"},
		{name: "key only: the key is the fallback", wantPort: "4407"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			beadsDir := writeMetadataFixture(t, `{"dolt_server_host":"10.0.0.5","dolt_server_port":4407}`, tt.portFile)

			meta := readDoltMetadata(beadsDir)
			if meta.Port != tt.wantPort {
				t.Errorf("Port = %q, want %q", meta.Port, tt.wantPort)
			}
		})
	}
}
