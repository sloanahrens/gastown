package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// hasArgPair reports whether args holds first immediately followed by second,
// past any leading global flag bd was given.
func hasArgPair(args []string, first, second string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == first && args[i+1] == second {
			return true
		}
	}
	return false
}

// bondArgvBD answers a sling's bd calls in memory and keeps the argv of every
// mol bond it ran, so a test can read the flags the bond was built with. all
// keeps every call, for a failure message that says what bd was asked.
type bondArgvBD struct {
	mu    sync.Mutex
	all   []string
	bonds [][]string
}

func (f *bondArgvBD) run(_ context.Context, args []string) ([]byte, []byte, error) {
	f.mu.Lock()
	f.all = append(f.all, strings.Join(args, " "))
	bond := hasArgPair(args, "mol", "bond")
	if bond {
		f.bonds = append(f.bonds, append([]string(nil), args...))
	}
	f.mu.Unlock()
	if bond {
		return []byte(`{"result_id":"gt-wisp-x"}`), nil, nil
	}
	return []byte(polecatWorkTree), nil, nil
}

// onlyBond is the one mol bond the sling ran; any other count fails the test.
func (f *bondArgvBD) onlyBond(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bonds) != 1 {
		t.Fatalf("the sling ran %d mol bonds, want 1; calls: %q", len(f.bonds), f.all)
	}
	return f.bonds[0]
}

// writeDaemonSettings writes body as the town's daemon settings file.
func writeDaemonSettings(t *testing.T, townRoot, body string) {
	t.Helper()
	path := config.DaemonPatrolConfigPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir the mayor dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write the daemon settings: %v", err)
	}
}

// TestSlingBondRootOnly: a sling bonds its formula with --root-only when the
// town's daemon settings ask for it (bond_root_only), and bonds every step as
// before when the key is off, absent, or the settings file cannot be read.
func TestSlingBondRootOnly(t *testing.T) {
	t.Parallel()
	const (
		on         = `{"type":"daemon-patrol-config","version":1,"bond_root_only":true}`
		off        = `{"type":"daemon-patrol-config","version":1,"bond_root_only":false}`
		absent     = `{"type":"daemon-patrol-config","version":1}`
		unreadable = "dir"
	)
	tests := []struct {
		name     string
		settings string // "" writes no settings file at all
		want     bool
	}{
		{"key true", on, true},
		{"key false", off, false},
		{"key absent", absent, false},
		{"settings file absent", "", false},
		{"settings file unreadable", unreadable, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			switch tc.settings {
			case "":
			case unreadable:
				// A directory where the settings file belongs makes the
				// read fail with something other than "not found".
				if err := os.MkdirAll(config.DaemonPatrolConfigPath(townRoot), 0o755); err != nil {
					t.Fatalf("mkdir over the settings path: %v", err)
				}
			default:
				writeDaemonSettings(t, townRoot, tc.settings)
			}

			fake := &bondArgvBD{}
			stores := slingStoresOn(nil, beads.WithRunner(fake.run))
			res, err := formulaBDFor(stores).instantiate(
				"mol-polecat-work", "gt-mmoj3w", "bond root-only", t.TempDir(), townRoot, nil)
			if err != nil {
				t.Fatalf("instantiate: %v; calls: %q", err, fake.all)
			}
			if res.WispRootID != "gt-wisp-x" {
				t.Errorf("wisp root = %q, want gt-wisp-x", res.WispRootID)
			}

			argv := fake.onlyBond(t)
			if got := slices.Contains(argv, "--root-only"); got != tc.want {
				t.Errorf("mol bond argv %q carries --root-only = %v, want %v", strings.Join(argv, " "), got, tc.want)
			}
		})
	}
}
