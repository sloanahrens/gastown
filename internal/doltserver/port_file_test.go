package doltserver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteDoltPortFile: the port file follows the port the caller passes. A
// port of 0 removes it; a file already absent is the state a removal wants, so
// it is no change and no error; a removal that fails for any other reason is
// returned, since the port file must not outlive the endpoint it named.
func TestWriteDoltPortFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		port        int
		setup       func(t *testing.T, portPath string)
		wantChanged bool
		wantErr     bool
		wantFile    string // the port file's content after the call; "" when it is absent
	}{
		{
			name:        "a present port file is removed",
			port:        0,
			setup:       func(t *testing.T, portPath string) { writeTestFile(t, portPath, "5432") },
			wantChanged: true,
		},
		{
			name:        "an absent port file is no change and no error",
			port:        0,
			wantChanged: false,
		},
		{
			name: "a removal that fails for another reason is returned",
			port: 0,
			setup: func(t *testing.T, portPath string) {
				// A non-empty directory cannot be removed, and it is not "absent".
				if err := os.Mkdir(portPath, 0o755); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(portPath, "inside"), "x")
			},
			wantErr: true,
		},
		{
			name:        "an unchanged port is no change",
			port:        5432,
			setup:       func(t *testing.T, portPath string) { writeTestFile(t, portPath, "5432") },
			wantChanged: false,
			wantFile:    "5432",
		},
		{
			name:        "a new port is written",
			port:        5432,
			wantChanged: true,
			wantFile:    "5432",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			beadsDir := t.TempDir()
			portPath := filepath.Join(beadsDir, "dolt-server.port")
			if tc.setup != nil {
				tc.setup(t, portPath)
			}
			changed, err := writeDoltPortFile(beadsDir, tc.port)
			if (err != nil) != tc.wantErr {
				t.Fatalf("writeDoltPortFile error = %v, want an error: %v", err, tc.wantErr)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if tc.wantErr {
				return
			}
			data, readErr := os.ReadFile(portPath)
			if tc.wantFile == "" {
				if !os.IsNotExist(readErr) {
					t.Errorf("port file should be absent, read %q: %v", data, readErr)
				}
				return
			}
			if readErr != nil || string(data) != tc.wantFile {
				t.Errorf("port file = %q (%v), want %q", data, readErr, tc.wantFile)
			}
		})
	}
}

// writeTestFile writes content to path, failing the test on error.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
