package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot locates the module root from this file's position, so the test
// reads the checked-in plugin regardless of the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// TestRebuildGTRunsOnTheDaemonPath pins rebuild-gt's one scheduling path
// (gt-o1z7): the daemon heartbeat dispatches cooldown-gate plugins itself
// (internal/daemon/handler.go, dispatchPlugins) and runs a script-type one
// in-process, so a cooldown gate plus `[execution] type = "script"` is what
// keeps the rebuild automatic.
//
// The park this guards is a real one: the runtime copy of this file read
// `gate: manual` from 2026-09-18, which took rebuild-gt off the automatic path
// — the daemon logged "skipping plugin rebuild-gt (gate=manual, requires
// explicit trigger)" on every heartbeat while the binary drifted — until the
// next `make install` re-synced the file from here. A manual gate committed
// here is the same park with nothing to undo it, and an agent-driven execution
// type would put a dog session back in front of every rebuild.
func TestRebuildGTRunsOnTheDaemonPath(t *testing.T) {
	path := filepath.Join(repoRoot(t), "plugins", "rebuild-gt", "plugin.md")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the test's own repo
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	scanner := NewScanner(repoRoot(t), nil)
	p, err := scanner.loadPlugin(filepath.Dir(path), LocationTown, "")
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	if p == nil {
		t.Fatalf("no plugin.md at %s", path)
	}

	if p.Gate == nil {
		t.Fatal("rebuild-gt has no gate: a nil gate reads as manual and is never auto-dispatched")
	}
	if p.Gate.Type != GateCooldown {
		t.Errorf("rebuild-gt gate type = %q, want %q: only cooldown gates are dispatched by the daemon", p.Gate.Type, GateCooldown)
	}
	if p.Gate.Duration == "" {
		// The handler checks the run count only when a duration is set, so an
		// empty one dispatches the rebuild on every heartbeat.
		t.Error("rebuild-gt cooldown gate has no duration: every heartbeat would dispatch it")
	}
	if p.Execution == nil || p.Execution.Type != ExecTypeScript {
		t.Errorf("rebuild-gt execution type = %v, want %q: the daemon runs run.sh in-process and dispatches a dog only on failure", p.Execution, ExecTypeScript)
	}
	if !p.HasRunScript {
		t.Error("rebuild-gt has no run.sh next to plugin.md: the daemon has nothing to execute")
	}

	// The hand-park itself, in the file an agent edits.
	if strings.Contains(string(raw), "type = \"manual\"") {
		t.Error("rebuild-gt declares a manual gate: that parks the automatic rebuild (gt-o1z7)")
	}
}
