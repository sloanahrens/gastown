package done

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestDoneLocalGateChecksNodeModulesBeforeSubmission is the gt done end of
// gt-wd12s: the pre-submit gate is built with the node_modules check on. It is
// the tier that can still repair the tree — the rebase has just brought the
// target's lockfile into a worktree whose node_modules has stood since before
// that lockfile landed, and the landing gate installs from the lockfile.
func TestDoneLocalGateChecksNodeModulesBeforeSubmission(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-Go rig needs its commands from the rig's own merge_queue.
	cfg := `{"type":"rig","version":1,"name":"rig","merge_queue":{"lint_command":"true"}}`
	if err := os.WriteFile(filepath.Join(townRoot, "rig", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	gate, err := doneLocalGate(townRoot, "rig", t.TempDir())
	if err != nil {
		t.Fatalf("doneLocalGate: %v", err)
	}
	g, ok := gate.(land.CommandGate)
	if !ok {
		t.Fatalf("doneLocalGate returned %T, want land.CommandGate", gate)
	}
	if !g.CheckNodeDeps {
		t.Error("gt done's pre-submit gate does not check node_modules against package-lock.json")
	}
}
