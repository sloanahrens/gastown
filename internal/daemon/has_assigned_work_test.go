package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestHasAssignedOpenWork_PinsTheRigDatabase(t *testing.T) {
	t.Parallel()

	// GetRigDirForName's pathWithin resolves symlinks on the town root but the
	// rig dir never exists here, so a symlinked TempDir (macOS /var ->
	// /private/var) would make it return "" and skip the pinned BEADS_DIR path.
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(townRoot, ".beads", "routes.jsonl"),
		[]byte("{\"prefix\":\"gt-\",\"path\":\"gastown/mayor/rig\"}\n"),
		0o644,
	); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	expectedBeadsDir := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{ID: "gt-123", Status: "hooked", Assignee: "polecats/rust"})

	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}

	has, err := d.hasAssignedOpenWork("gastown", "polecats/rust")
	if err != nil || !has {
		t.Fatalf("assigned work lookup = %v, %v; want true, nil", has, err)
	}

	if len(bd.envs) == 0 {
		t.Fatal("hasAssignedOpenWork made no bd read")
	}
	for _, env := range bd.envs {
		if got := (cliCall{env: env}).getenv("BEADS_DIR"); got != expectedBeadsDir {
			t.Fatalf("expected the read pinned to BEADS_DIR %q, got %q", expectedBeadsDir, got)
		}
	}
}
