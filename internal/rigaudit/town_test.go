package rigaudit

import (
	"bytes"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestTown checks that Town audits the town at townRoot and returns the audit's
// code: a readable clean town prints Report's OK line and returns 0, and a town
// whose config cannot be read prints the FAILED line and returns 1.
func TestTown(t *testing.T) {
	t.Parallel()
	const cleanTown = `{"version":1,"daemon":{"patrols":{"landing_worker":{"rigs":["alpha"]},"patrol_scan":{"rigs":["alpha"],"worktree_cleanup":{"rigs":["alpha"]}}}}}`
	const alphaSettings = `{"version":1,"merge_queue":{"forgejo":"forgejo","post_land_command":"make test-slow"}}`
	tests := []struct {
		name     string
		setup    func(*testing.T, string)
		wantOut  string
		wantCode int
	}{
		{"clean town", func(t *testing.T, root string) {
			writeRigs(t, root, `{"version":1,"rigs":{"alpha":{}}}`)
			writeConfig(t, root, "settings/config.json", cleanTown)
			writeConfig(t, root, "alpha/settings/config.json", alphaSettings)
		}, "RIG AUDIT: OK (1 unparked, 0 parked: none; 0 info)\n", 0},
		{"unreadable town config", func(t *testing.T, root string) {
			writeRigs(t, root, `{"version":1,"rigs":{"alpha":{}}}`)
		}, "RIG AUDIT: FAILED: ", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := testutil.PrivateTempDir(t)
			tc.setup(t, root)
			var out bytes.Buffer
			code := Town(root, func(string) string { return "" }, &out)
			got := out.String()
			if code != tc.wantCode || !strings.HasPrefix(got, tc.wantOut) {
				t.Errorf("Town() = %d, %q; want %d, %q", code, got, tc.wantCode, tc.wantOut)
			}
			if tc.wantCode != 0 && !strings.Contains(got, "settings/config.json") {
				t.Errorf("Town() = %q, want the FAILED reason to name settings/config.json", got)
			}
		})
	}
}
