package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The daemon-level decision: boot_mode unset or "mechanical" runs triage
// in-process; "agent" spawns the Boot session. Read through the same
// operational config loader ensureBootRunning uses.
func TestBootUsesMechanicalTriage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, config string
		want         bool
	}{
		{"no config file", "", true},
		{"boot_mode unset", `{"operational":{"daemon":{"boot_spawn_cooldown":"6m"}}}`, true},
		{"mechanical", `{"operational":{"daemon":{"boot_mode":"mechanical"}}}`, true},
		{"agent", `{"operational":{"daemon":{"boot_mode":"agent"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			town := t.TempDir()
			if tc.config != "" {
				if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), ctx: context.Background()}
			if got := d.bootUsesMechanicalTriage(); got != tc.want {
				t.Errorf("bootUsesMechanicalTriage() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A second call while a triage is in flight is a no-op, and the cooldown
// stamp is taken at start so a failing triage cannot rerun every heartbeat.
func TestRunMechanicalBootTriage_InFlightGuardAndCooldownStamp(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0), ctx: context.Background()}
	d.bootTriageInFlight.Store(true)
	d.runMechanicalBootTriage()
	if !d.bootLastSpawned.IsZero() {
		t.Error("a skipped (in-flight) triage must not move the cooldown stamp")
	}
}

// mechanicalTriageGt points d's mechanical triage at a fake `gt` and returns
// a channel that receives each `boot triage` call it answers, as `gt boot
// triage` answers when there is nothing to do.
func mechanicalTriageGt(d *Daemon) <-chan cliCall {
	triaged := make(chan cliCall, 1)
	d.bootTriageExeFn = func() (string, error) { return "/opt/gt/bin/gt", nil }
	d.execCmd = newFakeCLIFor(func(c cliCall) cliReply {
		if c.name == "gt" && strings.Join(c.args, " ") == "boot triage" {
			select {
			case triaged <- c:
			default:
			}
			return cliReply{stdout: "Triage complete: nothing\n"}
		}
		return cliReply{stderr: "unexpected call\n", code: 1}
	}).run
	return triaged
}

// awaitTriage waits for the async mechanical triage to run `gt boot triage`
// and checks it ran as Boot, from the deacon directory.
func awaitTriage(t *testing.T, townRoot string, triaged <-chan cliCall) {
	t.Helper()
	select {
	case c := <-triaged:
		if want := filepath.Join(townRoot, "deacon"); c.dir != want {
			t.Errorf("boot triage ran in %q, want %q", c.dir, want)
		}
		if got := c.getenv("GT_ROLE"); got != "deacon/boot" {
			t.Errorf("boot triage ran with GT_ROLE=%q, want deacon/boot", got)
		}
		if got := c.getenv("GT_TOWN_ROOT"); got != townRoot {
			t.Errorf("boot triage ran with GT_TOWN_ROOT=%q, want %q", got, townRoot)
		}
	case <-time.After(time.Minute):
		t.Fatal("mechanical triage did not run `gt boot triage`")
	}
}

// Default (mechanical) mode end to end: ensureBootRunning runs the stub
// `gt boot triage` instead of opening a tmux session, and stamps the cooldown.
// The stub records its argv so we know what would have run.
func TestEnsureBootRunning_MechanicalRunsTriageNoTmux(t *testing.T) {
	t.Parallel()
	townRoot, d, spawner := bootTestTown(t)
	triaged := mechanicalTriageGt(d)
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0o755); err != nil {
		t.Fatal(err)
	}

	d.ctx = context.Background()
	d.ensureBootRunning()

	awaitTriage(t, townRoot, triaged)
	if d.bootLastSpawned.IsZero() {
		t.Error("cooldown stamp not set by mechanical triage")
	}
	if n := spawner.count(); n != 0 {
		t.Errorf("mechanical mode must not open a Boot tmux session; spawns = %d", n)
	}
}

// Mechanical triage is in-process and pays no prefill, so the agent spawn
// cooldown must not gate it: gating it halves the default mode's triage
// cadence, and a Deacon that dies then waits twice as long to be noticed
// (gt-w28o).
func TestEnsureBootRunning_MechanicalIgnoresSpawnCooldown(t *testing.T) {
	t.Parallel()
	townRoot, d, _ := bootTestTown(t)
	triaged := mechanicalTriageGt(d)
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0o755); err != nil {
		t.Fatal(err)
	}

	d.ctx = context.Background()
	d.bootLastSpawned = time.Now() // a Boot spawn inside the cooldown window

	d.ensureBootRunning()

	awaitTriage(t, townRoot, triaged)
}

// Under go test the executable is the test binary; the mechanical path must
// refuse to exec it (that would re-run the suite recursively).
func TestRunMechanicalBootTriage_RefusesTestBinary(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(nil)
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0), ctx: context.Background(),
		bootTriageExeFn: func() (string, error) { return "/tmp/daemon.test", nil }, execCmd: gt.run}
	d.runMechanicalBootTriage()
	if d.bootTriageInFlight.Load() {
		t.Error("in-flight flag left set after refusing a test binary")
	}
	if calls := gt.recorded(); len(calls) != 0 {
		t.Errorf("the test binary was run: %+v", calls)
	}
}
