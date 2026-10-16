package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsAgentSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"hq-mayor", false}, // mayor role retired (gt-rwp7z.17)
		{"rig-crew-max", true},
		{"rig-refinery", false}, // refinery role removed (gt-v4ssj.6)
		{"rig-witness", false},  // witness role retired (gt-4k3fj.6.1)
		{"rig-polecat-abc", true},
		{"hq-deacon", false},
		{"hq-dog-fido", false}, // dog role retired (gt-29q6g)
		{"my-personal-session", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isAgentSession(tt.name); got != tt.want {
			t.Errorf("isAgentSession(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// countAgentSessions reads the daemon's own tmux (the town socket), and counts
// only the sessions that look like Gas Town agents.
func TestCountAgentSessionsCountsAgentsOnTheDaemonTmux(t *testing.T) {
	t.Parallel()
	tm := newFakeTmux(newFixedClock())
	for _, name := range []string{"rig-crew-max", "rig-polecat-abc", "my-personal-session"} {
		tm.addSession(name, "claude", time.Time{})
	}
	d := &Daemon{tmux: tm}
	if got := d.countAgentSessions(); got != 2 {
		t.Errorf("countAgentSessions = %d, want 2 (the personal session is not an agent)", got)
	}
}

// pressureDaemon is a Daemon whose town enables the CPU tier at threshold and
// whose host reads idle percent CPU idle (nil: top could not be read).
func pressureDaemon(t *testing.T, threshold float64, idle *float64) *Daemon {
	t.Helper()
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"type":"town-settings","version":1,"operational":{"daemon":{"pressure_cpu_threshold":%v}}}`, threshold)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d, _ := newTestDaemon(townRoot)
	d.cpuIdleFn = func() *float64 { return idle }
	return d
}

// The CPU tier judges the host by its idle CPU, not its load average: a
// 24-core macOS host at load 29.6 with 48.6% of its CPU idle is not
// CPU-pressured, and one with 4% idle is (gt-kc8v1).
func TestCheckPressureCPUTierJudgesHostByCPUIdle(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name      string
		threshold float64
		idle      *float64
		wantOK    bool
		wantIn    []string
	}{
		{"half the CPU idle spawns", 0.9, f(48.6), true, nil},
		{"idle exactly at the threshold spawns", 0.9, f(10), true, nil},
		{"under the idle the threshold needs holds and names the reading", 0.9, f(4.2), false, []string{"4.2% CPU idle", "0.96", "0.90"}},
		{"a top that could not be read is no evidence of a busy host", 0.9, nil, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := pressureDaemon(t, tt.threshold, tt.idle).checkPressure("polecat")
			if got.OK != tt.wantOK {
				t.Fatalf("OK = %v, want %v (reason %q)", got.OK, tt.wantOK, got.Reason)
			}
			for _, s := range tt.wantIn {
				if !strings.Contains(got.Reason, s) {
					t.Errorf("Reason %q does not name %q", got.Reason, s)
				}
			}
			if strings.Contains(got.Reason, "load") {
				t.Errorf("Reason %q still speaks of a load average", got.Reason)
			}
			if tt.idle != nil && got.CPUIdle != *tt.idle {
				t.Errorf("CPUIdle = %v, want %v", got.CPUIdle, *tt.idle)
			}
		})
	}
}

// With no threshold configured the CPU tier is dormant: top is never run.
func TestCheckPressureCPUTierDormantWithoutThreshold(t *testing.T) {
	t.Parallel()
	d, _ := newTestDaemon(t.TempDir())
	d.cpuIdleFn = func() *float64 {
		t.Error("top was read with every pressure tier disabled")
		return nil
	}
	if got := d.checkPressure("polecat"); !got.OK {
		t.Fatalf("OK = false with all tiers disabled: %q", got.Reason)
	}
}
