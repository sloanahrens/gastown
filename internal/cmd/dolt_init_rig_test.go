package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// `gt dolt init-rig` on a tracker rig returns without an error and leaves no
// database behind: InitRig made none, so there is no location to report
// (gt-p85pw.102). The line InitRig prints is not captured here: the unit tier
// may not swap process stdio, so this drives the call site itself.
//
//testpolicy:allow parallel — the cwd names the town under test, so the test chdirs the whole process
func TestDoltInitRigTrackerRig(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"),
		[]byte(`{"type":"town","version":2,"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	route := `{"prefix":"ag-","path":"agrig/mayor/rig","backend":"tracker"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}

	//testpolicy:allow no-chdir — the cwd names the town under test: runDoltInitRig finds it through workspace.FindFromCwdOrError
	t.Chdir(townRoot)

	if err := runDoltInitRig(doltInitRigCmd, []string{"agrig"}); err != nil {
		t.Fatalf("runDoltInitRig: %v", err)
	}
	if _, err := os.Stat(doltserver.RigDatabaseDir(townRoot, "agrig")); !os.IsNotExist(err) {
		t.Errorf("database directory for a tracker rig: %v, want it absent", err)
	}
}
