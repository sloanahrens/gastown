package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// writeTestPlugin writes a town-level plugin.md under townRoot/plugins/name.
func writeTestPlugin(t *testing.T, townRoot, name, pluginMD string) {
	t.Helper()
	dir := filepath.Join(townRoot, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.md"), []byte(pluginMD), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// A manual-gate plugin never runs from the heartbeat, and a plugin with
// nothing to execute is logged as skipped: with the dog pack retired
// (gt-ckunw) nothing runs it, and the log line is what keeps that visible.
// Neither starts a script run.
func TestDispatchPlugins_RunsOnlyScriptPlugins(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	logs := &lockedBuffer{}
	bash := newFakeCLIFor(func(cliCall) cliReply { return cliReply{} })
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(logs, "", 0), execCmd: bash.run}

	writeTestPlugin(t, townRoot, "test-manual",
		"+++\nname = \"test-manual\"\ndescription = \"manual gate plugin\"\n\n[gate]\ntype = \"manual\"\n+++\n\n# Instructions\n")
	writeTestPlugin(t, townRoot, "test-agent",
		"+++\nname = \"test-agent\"\ndescription = \"instructions only\"\n\n[gate]\ntype = \"cooldown\"\n+++\n\n# Instructions\n")

	d.dispatchPlugins(&config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}})

	out := logs.String()
	if !strings.Contains(out, "skipping plugin test-manual (gate=manual") {
		t.Errorf("manual-gate plugin not skipped:\n%s", out)
	}
	if !strings.Contains(out, "skipping plugin test-agent (not a script plugin") {
		t.Errorf("non-script plugin not reported as skipped:\n%s", out)
	}
	if n := len(bash.recorded()); n != 0 {
		t.Errorf("%d script runs started, want none", n)
	}
}

// A town whose mayor/rigs.json is missing or invalid still dispatches the
// plugins that need no rig; only the rig-level scan is lost.
func TestHandlePlugins_MissingRigsConfigStillDispatchesTownPlugins(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	logs := &lockedBuffer{}
	bash := newFakeCLIFor(func(cliCall) cliReply { return cliReply{} })
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(logs, "", 0), execCmd: bash.run}

	writeTestPlugin(t, townRoot, "test-manual",
		"+++\nname = \"test-manual\"\ndescription = \"manual gate plugin\"\n\n[gate]\ntype = \"manual\"\n+++\n\n# Instructions\n")

	d.handlePlugins()

	out := logs.String()
	if !strings.Contains(out, "failed to load rigs config") {
		t.Errorf("rigs config failure not logged:\n%s", out)
	}
	if !strings.Contains(out, "skipping plugin test-manual (gate=manual") {
		t.Errorf("town plugins not dispatched without a rigs config:\n%s", out)
	}
}

// writeTestScriptPlugin writes a town-level cooldown plugin with a run.sh, the
// shape the daemon executes itself.
func writeTestScriptPlugin(t *testing.T, townRoot, name string) {
	t.Helper()
	writeTestPlugin(t, townRoot, name,
		"+++\nname = \""+name+"\"\ndescription = \"script plugin\"\n\n[gate]\ntype = \"cooldown\"\n\n[execution]\ntype = \"script\"\ntimeout = \"5s\"\n+++\n\n# Instructions\n")
	if err := os.WriteFile(filepath.Join(townRoot, "plugins", name, "run.sh"), []byte("exit 0\n"), 0o644); err != nil {
		t.Fatalf("WriteFile run.sh: %v", err)
	}
}

// With no rigs config (handlePlugins passes nil when mayor/rigs.json cannot be
// loaded) a due town script plugin still starts: the nil config costs the
// rig-level scan, not the run. The fake cancels the daemon's context during
// the run, so the run is an abort and records nothing.
func TestDispatchPlugins_NilRigsConfigStillStartsTownScriptPlugin(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bash := newFakeCLIFor(func(cliCall) cliReply {
		cancel() // the daemon stopped mid-run: an abort writes no run record
		return cliReply{code: 1}
	})
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(logs, "", 0), ctx: ctx, execCmd: bash.run}

	writeTestScriptPlugin(t, townRoot, "town-script")

	d.dispatchPlugins(nil)
	d.scripts.wait()

	calls := bash.recorded()
	if len(calls) != 1 || calls[0].name != "bash" || !slices.Equal(calls[0].args, []string{"run.sh"}) {
		t.Fatalf("ran %+v, want one `bash run.sh`", calls)
	}
	if !strings.Contains(logs.String(), "running script plugin town-script directly") {
		t.Errorf("script start not logged:\n%s", logs.String())
	}
}
