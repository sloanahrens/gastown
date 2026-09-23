package plugin

import (
	"path/filepath"
	"runtime"
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
// A hand-park is the failure this guards against: an agent that edits this
// plugin directly under `<town_root>/plugins` — the daemon's runtime copy —
// leaves the repo untouched until `gt plugin sync` overwrites it. What the
// test can catch is the committed state, and the parsed checks below do.
func TestRebuildGTRunsOnTheDaemonPath(t *testing.T) {
	path := filepath.Join(repoRoot(t), "plugins", "rebuild-gt", "plugin.md")

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
}
