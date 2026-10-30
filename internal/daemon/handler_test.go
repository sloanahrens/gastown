package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/plugin"
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

// writeTestCooldownScriptPlugin writes a town-level script plugin whose
// cooldown gate looks back duration.
func writeTestCooldownScriptPlugin(t *testing.T, townRoot, name, duration string) {
	t.Helper()
	writeTestPlugin(t, townRoot, name,
		"+++\nname = \""+name+"\"\ndescription = \"script plugin\"\n\n[gate]\ntype = \"cooldown\"\nduration = \""+duration+"\"\n\n[execution]\ntype = \"script\"\ntimeout = \"5s\"\n+++\n\n# Instructions\n")
	if err := os.WriteFile(filepath.Join(townRoot, "plugins", name, "run.sh"), []byte("exit 0\n"), 0o644); err != nil {
		t.Fatalf("WriteFile run.sh: %v", err)
	}
}

// countingClient counts the List calls a Recorder makes, so a test can price
// a tick's ledger reads.
type countingClient struct {
	beads.Client
	lists   int
	cutoffs []time.Time
}

func (c *countingClient) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	c.lists++
	c.cutoffs = append(c.cutoffs, opts.CreatedAfter)
	return c.Client.List(opts)
}

// failingClient is a beads client whose reads fail.
type failingClient struct{ beads.Client }

func (failingClient) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("bd: read failed")
}

// fakeRunSource is the tick's plugin run ledger with canned gate answers, for
// the failure a real ledger cannot produce: a Count error.
type fakeRunSource struct {
	ledger *fakeRunLedger
}

func (s *fakeRunSource) LoadRunLedger(time.Duration) (pluginRunLedger, error) {
	return s.ledger, nil
}

func (s *fakeRunSource) RecordRun(plugin.PluginRunRecord) (string, error) {
	return "wisp-test", nil
}

// fakeRunLedger answers each plugin's cooldown gate from canned counts and
// errors.
type fakeRunLedger struct {
	counts map[string]int
	errs   map[string]error
}

func (l *fakeRunLedger) Count(pluginName string, _ time.Duration) (int, error) {
	if err := l.errs[pluginName]; err != nil {
		return 0, err
	}
	return l.counts[pluginName], nil
}

// startedPlugins names the plugins whose run.sh a tick started, from the
// directory each run ran in.
func startedPlugins(calls []cliCall) []string {
	var ran []string
	for _, c := range calls {
		ran = append(ran, filepath.Base(c.dir))
	}
	slices.Sort(ran)
	return ran
}

// Every cooldown gate in a tick is answered from one read of the run ledger,
// back through the longest window any of them uses. Listing the ledger once
// per plugin was one bd call per plugin per heartbeat (gt-3bqfy).
func TestDispatchPlugins_OneLedgerReadAnswersEveryCooldownGate(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bash := newFakeCLIFor(func(cliCall) cliReply {
		cancel() // the daemon stopped mid-run: an abort writes no run record
		return cliReply{code: 1}
	})
	runs := &countingClient{Client: beadsfake.New()}
	d := &Daemon{
		config:     &Config{TownRoot: townRoot},
		logger:     log.New(logs, "", 0),
		ctx:        ctx,
		execCmd:    bash.run,
		pluginRuns: recorderSource{plugin.NewRecorderOn(runs)},
	}

	writeTestCooldownScriptPlugin(t, townRoot, "gate-30m", "30m")
	writeTestCooldownScriptPlugin(t, townRoot, "gate-1h", "1h")
	writeTestCooldownScriptPlugin(t, townRoot, "gate-2h", "2h")

	before := time.Now()
	d.dispatchPlugins(nil)
	d.scripts.wait()

	if runs.lists != 1 {
		t.Errorf("%d ledger list(s) for 3 gated plugins, want 1", runs.lists)
	}
	if len(runs.cutoffs) != 1 {
		t.Fatalf("%d ledger reads, want 1", len(runs.cutoffs))
	}
	if want := before.Add(-2 * time.Hour); runs.cutoffs[0].Before(want.Add(-time.Minute)) || runs.cutoffs[0].After(want.Add(time.Minute)) {
		t.Errorf("ledger read back to %s, want the longest gate's 2h before the tick (%s)", runs.cutoffs[0], want)
	}
	if ran := startedPlugins(bash.recorded()); !slices.Equal(ran, []string{"gate-1h", "gate-2h", "gate-30m"}) {
		t.Errorf("started %v, want all three gate-open plugins:\n%s", ran, logs.String())
	}
}

// A plugin with a run inside its cooldown window is held for this tick; the
// plugin with an empty ledger runs.
func TestDispatchPlugins_CooldownGateHoldsPluginWithARecentRun(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bash := newFakeCLIFor(func(cliCall) cliReply {
		cancel()
		return cliReply{code: 1}
	})
	clock := clockwork.NewFakeClockAt(time.Now().UTC())
	recorder := plugin.NewRecorderOn(beadsfake.New(beadsfake.WithClock(clock)))
	if _, err := recorder.RecordRun(plugin.PluginRunRecord{PluginName: "cooling", Result: plugin.ResultSuccess}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	d := &Daemon{
		config:     &Config{TownRoot: townRoot},
		logger:     log.New(logs, "", 0),
		ctx:        ctx,
		execCmd:    bash.run,
		pluginRuns: recorderSource{recorder},
	}

	writeTestCooldownScriptPlugin(t, townRoot, "cooling", "1h")
	writeTestCooldownScriptPlugin(t, townRoot, "due", "1h")

	d.dispatchPlugins(nil)
	d.scripts.wait()

	if ran, want := startedPlugins(bash.recorded()), []string{"due"}; !slices.Equal(ran, want) {
		t.Errorf("started %v, want %v: cooling ran inside its window, due did not", ran, want)
	}
}

// A gate the ledger cannot answer holds its plugin too: a cooldown whose
// state is unknown is not known to be open.
func TestDispatchPlugins_UnreadableGateFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bash := newFakeCLIFor(func(cliCall) cliReply {
		cancel()
		return cliReply{code: 1}
	})
	source := &fakeRunSource{ledger: &fakeRunLedger{errs: map[string]error{"broken": errors.New("bd: read failed")}}}
	d := &Daemon{
		config:     &Config{TownRoot: townRoot},
		logger:     log.New(logs, "", 0),
		ctx:        ctx,
		execCmd:    bash.run,
		pluginRuns: source,
	}

	writeTestCooldownScriptPlugin(t, townRoot, "broken", "1h")
	writeTestCooldownScriptPlugin(t, townRoot, "due", "1h")

	d.dispatchPlugins(nil)
	d.scripts.wait()

	if ran, want := startedPlugins(bash.recorded()), []string{"due"}; !slices.Equal(ran, want) {
		t.Errorf("started %v, want %v:\n%s", ran, want, logs.String())
	}
	if !strings.Contains(logs.String(), "error checking cooldown for plugin broken") {
		t.Errorf("unreadable gate not logged:\n%s", logs.String())
	}
}

// When the ledger cannot be read at all, the gated plugins wait a tick rather
// than run on state nobody could read; a gate with no duration still runs.
func TestDispatchPlugins_LedgerReadFailureSkipsGatedPlugins(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bash := newFakeCLIFor(func(cliCall) cliReply {
		cancel()
		return cliReply{code: 1}
	})
	d := &Daemon{
		config:     &Config{TownRoot: townRoot},
		logger:     log.New(logs, "", 0),
		ctx:        ctx,
		execCmd:    bash.run,
		pluginRuns: recorderSource{plugin.NewRecorderOn(failingClient{})},
	}

	writeTestCooldownScriptPlugin(t, townRoot, "gated", "1h")
	writeTestScriptPlugin(t, townRoot, "always") // cooldown gate, no duration: nothing to gate on

	d.dispatchPlugins(nil)
	d.scripts.wait()

	if ran, want := startedPlugins(bash.recorded()), []string{"always"}; !slices.Equal(ran, want) {
		t.Errorf("started %v, want %v", ran, want)
	}
	if !strings.Contains(logs.String(), "failed to read the plugin run ledger") {
		t.Errorf("ledger read failure not logged:\n%s", logs.String())
	}
}

// A cooldown gate whose duration is zero or negative reaches back to now, so
// no run holds it closed: the plugin runs on every tick, as it did when
// CountRunsSince counted from now, and it costs the tick no ledger read.
func TestDispatchPlugins_ZeroCooldownRunsWithoutALedgerRead(t *testing.T) {
	for _, duration := range []string{"0", "-5m"} {
		t.Run(duration, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			logs := &lockedBuffer{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bash := newFakeCLIFor(func(cliCall) cliReply {
				cancel()
				return cliReply{code: 1}
			})
			runs := &countingClient{Client: beadsfake.New()}
			d := &Daemon{
				config:     &Config{TownRoot: townRoot},
				logger:     log.New(logs, "", 0),
				ctx:        ctx,
				execCmd:    bash.run,
				pluginRuns: recorderSource{plugin.NewRecorderOn(runs)},
			}

			writeTestCooldownScriptPlugin(t, townRoot, "every-tick", duration)

			d.dispatchPlugins(nil)
			d.scripts.wait()

			if ran, want := startedPlugins(bash.recorded()), []string{"every-tick"}; !slices.Equal(ran, want) {
				t.Errorf("started %v, want %v: a %q window holds nothing closed", ran, want, duration)
			}
			if runs.lists != 0 {
				t.Errorf("%d ledger list(s), want 0: a window of %q asks the ledger nothing", runs.lists, duration)
			}
		})
	}
}
