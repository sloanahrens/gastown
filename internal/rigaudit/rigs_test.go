package rigaudit

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestReadRigs checks the town's registered rigs split into unparked and
// parked, and the error a registry that cannot be read or parsed returns.
// The unreadable row puts a directory where the registry file belongs, so it
// fails as root too.
func TestReadRigs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		setup        func(t *testing.T, townRoot string)
		state        map[string]string
		wantUnparked []string
		wantParked   []string
		wantErr      bool
		notInErr     string
	}{
		{
			name: "three rigs with one parked",
			setup: func(t *testing.T, townRoot string) {
				writeRigs(t, townRoot, `{"version":1,"rigs":{"alpha":{},"bravo":{},"charlie":{}}}`)
			},
			state:        map[string]string{"bravo": "PARKED"},
			wantUnparked: []string{"alpha", "charlie"},
			wantParked:   []string{"bravo"},
		},
		{
			name:  "missing registry is an empty town",
			setup: func(t *testing.T, townRoot string) {},
		},
		{
			name: "unreadable registry is an error",
			setup: func(t *testing.T, townRoot string) {
				if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rigs.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "unparseable registry is an error without its contents",
			setup: func(t *testing.T, townRoot string) {
				writeRigs(t, townRoot, `{"version":1,"rigs":{"untrusted-marker":`)
			},
			wantErr:  true,
			notInErr: "untrusted-marker",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			tc.setup(t, townRoot)
			unparked, parked, err := ReadRigs(townRoot, func(rig string) string { return tc.state[rig] })
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ReadRigs = %v, %v, <nil>, want an error", unparked, parked)
				}
				if tc.notInErr != "" && strings.Contains(err.Error(), tc.notInErr) {
					t.Fatalf("error carries the file's contents %q: %v", tc.notInErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadRigs: %v", err)
			}
			if !slices.Equal(unparked, tc.wantUnparked) {
				t.Errorf("unparked = %v, want %v", unparked, tc.wantUnparked)
			}
			if !slices.Equal(parked, tc.wantParked) {
				t.Errorf("parked = %v, want %v", parked, tc.wantParked)
			}
		})
	}
}

// writeRigs writes data as the town's mayor/rigs.json.
func writeRigs(t *testing.T, townRoot, data string) {
	t.Helper()
	dir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rigs.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
