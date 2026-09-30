package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// writeTestPlugin writes a town-level plugin.md (and, when script is set, a
// run.sh) under townRoot/plugins/name.
func writeTestPlugin(t *testing.T, townRoot, name, pluginMD, script string) {
	t.Helper()
	dir := filepath.Join(townRoot, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.md"), []byte(pluginMD), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if script != "" {
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
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
		"+++\nname = \"test-manual\"\ndescription = \"manual gate plugin\"\n\n[gate]\ntype = \"manual\"\n+++\n\n# Instructions\n", "")
	writeTestPlugin(t, townRoot, "test-agent",
		"+++\nname = \"test-agent\"\ndescription = \"instructions only\"\n\n[gate]\ntype = \"cooldown\"\n+++\n\n# Instructions\n", "")

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
