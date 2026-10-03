package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// TestRigBeadsCheckFix_UnparseableRigConfigReports pins gt-8xk9k for the
// doctor fix path: a rig config.json that no longer decodes is reported once,
// and the git URL it feeds the identity bead stays empty — the fallback this
// call site always had. The structured finding remains RigConfigSyncCheck's;
// this is the generic once-per-rig warning.
func TestRigBeadsCheckFix_UnparseableRigConfigReports(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(`{"prefix":"tr-","path":"testrig/mayor/rig"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	rigPath := filepath.Join(townRoot, "testrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"type":"rig","version":1,"name":"testrig","git_urll":"https://example.invalid/x.git"}`), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	town := newDoctorTown()
	if err := NewRigBeadsCheck().Fix(town.ctx(townRoot, "")); err != nil {
		t.Fatalf("RigBeadsCheck.Fix: %v", err)
	}
	if !rig.RigConfigWarned(rigPath) {
		t.Errorf("RigBeadsCheck.Fix did not report the unparseable config.json")
	}

	// The identity bead was still created, with no repo line: the empty git
	// URL fallback is unchanged.
	beadsPath := filepath.Join(townRoot, "testrig", "mayor", "rig")
	issue, err := town.db(beadsPath).Show("tr-rig-testrig")
	if err != nil {
		t.Fatalf("show created rig bead: %v", err)
	}
	if strings.Contains(issue.Description, "repo:") {
		t.Errorf("rig bead repo = %q; want the empty fallback", issue.Description)
	}
}
