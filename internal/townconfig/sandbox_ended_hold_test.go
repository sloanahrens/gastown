package townconfig

import (
	"path/filepath"
	"strings"
	"testing"
)

// A sandbox profile's ended hold is checked at load against the patrol_scan
// interval in the daemon file: a hold below three intervals fails the load and
// names the settings file and the key, and the default passes at the town's
// 10m tick (gt-yck7hz).
func TestSandboxEndedHoldIsCheckedAtLoad(t *testing.T) {
	t.Parallel()
	const daemon = `{"type":"daemon-patrol-config","version":1,"patrols":{"patrol_scan":{"enabled":true,"interval":"10m"}}}`
	profile := func(hold string) string {
		return `{"type":"town-settings","version":1,"sandboxes":{"polecat-default":{"gateway":"k8s","image":"img","policy":"p.yaml"` + hold + `}}}`
	}
	tests := []struct {
		name     string
		settings string
		wantErr  string // a substring the load error carries; "" for a clean load
	}{
		{"a hold below three intervals fails the load", profile(`,"ended_hold_seconds":300`), "sandboxes.polecat-default.ended_hold_seconds 300: want at least 1800"},
		{"a hold of three intervals loads", profile(`,"ended_hold_seconds":1800`), ""},
		{"an unset hold loads at the 10m tick", profile(""), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := copyFixtureTown(t)
			write(t, root, FileSettings, tt.settings)
			write(t, root, FileDaemon, daemon)
			_, err := Load(root)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Load = %v, want a clean load", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load = nil, want a refusal carrying %q", tt.wantErr)
			}
			if msg := err.Error(); !strings.Contains(msg, tt.wantErr) || !strings.Contains(msg, filepath.Join(root, FileSettings)) {
				t.Fatalf("refusal %q must carry %q and name settings/config.json", msg, tt.wantErr)
			}
		})
	}
}
