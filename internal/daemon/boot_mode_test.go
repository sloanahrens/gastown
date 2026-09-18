package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// The daemon-level decision: boot_mode unset or "mechanical" runs triage
// in-process; "agent" spawns the Boot session. Read through the same
// operational config loader ensureBootRunning uses.
func TestBootUsesMechanicalTriage(t *testing.T) {
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
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0), ctx: context.Background()}
	d.bootTriageInFlight.Store(true)
	d.runMechanicalBootTriage()
	if !d.bootLastSpawned.IsZero() {
		t.Error("a skipped (in-flight) triage must not move the cooldown stamp")
	}
}

// Default (mechanical) mode end to end: ensureBootRunning runs the stub
// `gt boot triage` instead of opening a tmux session, and stamps the
// cooldown. The stub records its argv so we know what would have run.
func TestEnsureBootRunning_MechanicalRunsTriageNoTmux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binaries need bash")
	}
	townRoot := t.TempDir()
	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeTmux(t, fakeBinDir)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", tmuxLog)
	t.Setenv("GT_DEGRADED", "false")
	if err := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0o755); err != nil {
		t.Fatal(err)
	}

	argvLog := filepath.Join(t.TempDir(), "argv.log")
	stub := filepath.Join(fakeBinDir, "gt")
	if err := os.WriteFile(stub, []byte("#!/bin/bash\necho \"$@\" >> "+argvLog+"\necho 'Triage complete: nothing'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := bootTriageExecutable
	bootTriageExecutable = func() (string, error) { return stub, nil }
	t.Cleanup(func() { bootTriageExecutable = orig })

	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(io.Discard, "", 0), tmux: tmux.NewTmux(), ctx: context.Background()}
	d.ensureBootRunning()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(argvLog); strings.Contains(string(b), "boot triage") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(b), "boot triage") {
		t.Fatalf("mechanical mode did not run `gt boot triage`; argv log: %q", string(b))
	}
	if d.bootLastSpawned.IsZero() {
		t.Error("cooldown stamp not set by mechanical triage")
	}
	data, _ := os.ReadFile(tmuxLog)
	if strings.Contains(string(data), "new-session") {
		t.Errorf("mechanical mode must not open a Boot tmux session; tmux log: %s", data)
	}
}

// Under go test the executable is the test binary; the mechanical path must
// refuse to exec it (that would re-run the suite recursively).
func TestRunMechanicalBootTriage_RefusesTestBinary(t *testing.T) {
	orig := bootTriageExecutable
	bootTriageExecutable = func() (string, error) { return "/tmp/daemon.test", nil }
	t.Cleanup(func() { bootTriageExecutable = orig })
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0), ctx: context.Background()}
	d.runMechanicalBootTriage()
	if d.bootTriageInFlight.Load() {
		t.Error("in-flight flag left set after refusing a test binary")
	}
}
