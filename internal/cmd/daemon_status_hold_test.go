package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/templates"
)

// TestDaemonStatus_DispatchHoldLine: gt daemon status prints the dispatch hold
// line in both forms, clear for a town with no hold and held for a town whose
// operator hold file is present.
func TestDaemonStatus_DispatchHoldLine(t *testing.T) {
	t.Parallel()
	holdFile := func(t *testing.T, town string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(town, dispatch.HoldFileName), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	noHold := func(*testing.T, string) {}
	tests := []struct {
		name    string
		hold    func(t *testing.T, town string)
		running bool
		want    string
	}{
		{"not held, not running", noHold, false, "Dispatch hold: clear\n"},
		{"held, not running", holdFile, false, "Dispatch hold: operator dispatch hold present ("},
		{"not held, running", noHold, true, "Dispatch hold: clear\n"},
		{"held, running", holdFile, true, "Dispatch hold: operator dispatch hold present ("},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := stubSupervisor(t, "darwin", "", templates.SupervisorState{}, nil)
			c.isRunning = func(string) (bool, int, error) {
				if tt.running {
					return true, 4242, nil
				}
				return false, 0, nil
			}
			town := t.TempDir()
			tt.hold(t, town)

			var out strings.Builder
			err := c.daemonStatus(&out, town)
			if tt.running && err != nil {
				t.Fatalf("daemonStatus with a running daemon: %v", err)
			}
			if !tt.running {
				var silent *SilentExitError
				if !errors.As(err, &silent) || silent.Code != 3 {
					t.Fatalf("daemonStatus with no daemon: err = %v, want SilentExitError code 3", err)
				}
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("status output does not contain %q:\n%s", tt.want, out.String())
			}
		})
	}
}
