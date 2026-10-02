package beads

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// townWithRoutes is a town root whose routes claim gt- for the rig workspace
// named by the returned path.
func townWithRoutes(t *testing.T) (townRoot, rigWorktree string) {
	t.Helper()
	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	townBeads := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeads, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDatabaseMetadata(t, townBeads, "hq")
	routes := `{"prefix":"hq-","path":"."}` + "\n" + `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}

	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeads, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDatabaseMetadata(t, rigBeads, "gt")

	rigWorktree = filepath.Join(townRoot, "gastown", "polecats", "nux", "gastown")
	if err := os.MkdirAll(filepath.Join(rigWorktree, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigWorktree, ".beads", "redirect"), []byte("../../../mayor/rig/.beads\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot, rigWorktree
}

func writeDatabaseMetadata(t *testing.T, beadsDir, database string) {
	t.Helper()
	writeMetadata(t, beadsDir, map[string]any{
		"backend":          "dolt",
		"dolt_mode":        "server",
		"dolt_server_host": "127.0.0.1",
		"dolt_server_port": 3307,
		"dolt_database":    database,
	})
}

// TestSubprocessModeForCallPinsIDLessCalls covers the rule that keeps an
// ID-less bd call on the workspace it was placed in: routing can only read a
// target from an ID in argv (gt-tvld5).
func TestSubprocessModeForCallPinsIDLessCalls(t *testing.T) {
	t.Parallel()
	townRoot, rigWorktree := townWithRoutes(t)
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")

	tests := []struct {
		name     string
		workDir  string
		args     []string
		wantMode SubprocessEnvMode
		wantDir  string
	}{
		{"kv list from the worktree", rigWorktree, []string{"kv", "list", "--json"}, ReadOnlyPinned, rigBeads},
		{"label list from the town root", townRoot, []string{"list", "--status=open", "--label=gt:escalation", "--json"}, ReadOnlyPinned, filepath.Join(townRoot, ".beads")},
		{"create pins to the workspace", rigWorktree, []string{"create", "--title=x"}, MutationPinned, rigBeads},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mode, dir, err := SubprocessModeForCall(tc.workDir, tc.args)
			if err != nil {
				t.Fatalf("SubprocessModeForCall: %v", err)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %v, want %v", mode, tc.wantMode)
			}
			if dir != tc.wantDir {
				t.Errorf("dir = %q, want %q", dir, tc.wantDir)
			}
		})
	}
}

// An argv naming a bead routes: the ID itself selects the database, and naming
// one here would override routing (gt-tvld5).
func TestSubprocessModeForCallRoutesWhenArgvNamesABead(t *testing.T) {
	t.Parallel()
	townRoot, _ := townWithRoutes(t)

	mode, _, err := SubprocessModeForCall(townRoot, []string{"show", "gt-abc", "--json"})
	if err != nil {
		t.Fatalf("SubprocessModeForCall: %v", err)
	}
	if mode != ReadOnlyRouting {
		t.Errorf("mode = %v, want ReadOnlyRouting", mode)
	}
}

// A workspace directory that exists and names no database is refused here, so
// bd never reaches its built-in default "beads" (gt-170zk).
func TestSubprocessModeForCallRefusesNamelessWorkspace(t *testing.T) {
	t.Parallel()
	townRoot, _ := townWithRoutes(t)
	nameless := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	if err := os.Remove(filepath.Join(nameless, "metadata.json")); err != nil {
		t.Fatal(err)
	}

	_, _, err := SubprocessModeForCall(filepath.Join(townRoot, "gastown", "polecats", "nux", "gastown"), []string{"kv", "list"})
	if !errors.Is(err, ErrNoConfiguredDatabase) {
		t.Fatalf("err = %v, want ErrNoConfiguredDatabase", err)
	}
}

// A workDir with no .beads of its own has no workspace to name or refuse: bd
// keeps resolving upward from its cwd, which is how a town-level agent's
// directory reaches the town database (gt-tvld5).
func TestSubprocessModeForCallLeavesAnAbsentWorkspaceToBd(t *testing.T) {
	t.Parallel()
	townRoot, _ := townWithRoutes(t)
	mayorDir := filepath.Join(townRoot, "mayor")

	mode, dir, err := SubprocessModeForCall(mayorDir, []string{"kv", "list"})
	if err != nil {
		t.Fatalf("SubprocessModeForCall: %v", err)
	}
	if mode != ReadOnlyRouting {
		t.Errorf("mode = %v, want ReadOnlyRouting", mode)
	}
	if want := filepath.Join(mayorDir, ".beads"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}
