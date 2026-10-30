package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// `gt dolt init-rig` on a tracker rig returns without an error, leaves no
// database behind and says why: InitRig made none, so there is no location to
// report and "already exists" would name a database this rig never had
// (gt-p85pw.102).
func TestDoltInitRigTrackerRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	route := `{"prefix":"ag-","path":"agrig/mayor/rig","backend":"tracker"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := doltInitRig(&out, townRoot, "agrig"); err != nil {
		t.Fatalf("doltInitRig: %v", err)
	}
	if _, err := os.Stat(doltserver.RigDatabaseDir(townRoot, "agrig")); !os.IsNotExist(err) {
		t.Errorf("database directory for a tracker rig: %v, want it absent", err)
	}
	if got := out.String(); !strings.Contains(got, "tracker backend") || !strings.Contains(got, "agrig") {
		t.Errorf("output = %q, want the reason the rig has no database here", got)
	}
}
