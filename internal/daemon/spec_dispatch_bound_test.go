package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// A patrol block that cannot be read leaves the tick bound at the one-sling
// size, and the log says so once per distinct error rather than every tick
// (gt-zeqqg).
func TestSpecDispatchTickBoundLogsAnUnreadablePatrolConfig(t *testing.T) {
	t.Parallel()
	logger, lines := lineLogger()
	townRoot := t.TempDir()
	path := PatrolConfigFile(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: logger}

	for range 3 {
		if got, want := d.specDispatchTickBound(), specDispatchBound(specDispatchTimeout, 1); got != want {
			t.Fatalf("bound with an unreadable block = %v, want the one-sling bound %v", got, want)
		}
	}
	if n := countContaining(lines(), "patrol config unreadable", "one-sling"); n != 1 {
		t.Errorf("unreadable-config lines over three bounds = %d, want one: %v", n, lines())
	}

	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{MaxPerTick: 3}}}
	// The writer refuses to replace a file that does not parse.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := SavePatrolConfig(townRoot, cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := d.specDispatchTickBound(), specDispatchBound(specDispatchTimeout, 3); got != want {
		t.Errorf("bound after the block is readable = %v, want %v", got, want)
	}
	if n := countContaining(lines(), "patrol config readable again"); n != 1 {
		t.Errorf("readable-again lines = %d, want one: %v", n, lines())
	}
}

// A tick reads its bound once: the failure line names the bound the command ran
// under even when the patrol block changes while the tick runs (gt-zeqqg).
func TestSpecDispatchFailureNamesTheBoundTheTickRanUnder(t *testing.T) {
	t.Parallel()
	logger, lines := lineLogger()
	var d *Daemon
	fake := newFakeCLI(func([]string) cliReply {
		cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{MaxPerTick: 4}}}
		if err := SavePatrolConfig(d.config.TownRoot, cfg); err != nil {
			t.Error(err)
		}
		return cliReply{code: 1, stderr: "signal: killed"}
	})
	d = specDispatchFake(t, fake, logger)

	d.runSpecDispatch()

	ran := specDispatchBound(specDispatchTimeout, 1).String() + " bound"
	if n := countContaining(lines(), "spec_dispatch: tick failed after", ran); n != 1 {
		t.Errorf("failure line naming the bound the tick ran under = %d, want one: %v", n, lines())
	}
}

// The hooked line sling.Run prints and the count the ticker takes of it share
// one constant (gt-zeqqg).
func TestSpecSlingsHookedCountsTheSharedMarker(t *testing.T) {
	t.Parallel()
	out := strings.Repeat("  ✓ "+dispatch.SlingHookedMarker+"p\n", 3)
	if got := specSlingsHooked([]byte(out)); got != 3 {
		t.Errorf("slings hooked = %d, want 3", got)
	}
}
