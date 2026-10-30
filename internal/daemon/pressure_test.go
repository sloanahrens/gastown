package daemon

import (
	"context"
	"errors"
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
	if got, err := d.countAgentSessions(); err != nil || got != 2 {
		t.Errorf("countAgentSessions = %d, %v, want 2 (the personal session is not an agent)", got, err)
	}
}

// pressureDaemon is a Daemon whose town enables the CPU tier at threshold and
// whose host reads idle percent CPU idle (or readErr when top could not be
// read). It returns the daemon's log with it.
func pressureDaemon(t *testing.T, threshold float64, idle float64, readErr error) (*Daemon, *strings.Builder) {
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
	d, logs := newTestDaemon(townRoot)
	d.cpuIdleFn = func() (float64, error) { return idle, readErr }
	return d, logs
}

// The CPU tier judges the host by its idle CPU, not its load average: a
// 24-core macOS host at load 29.6 with 48.6% of its CPU idle is not
// CPU-pressured, and one with 4% idle is (gt-kc8v1).
func TestCheckPressureCPUTierJudgesHostByCPUIdle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		threshold float64
		idle      float64
		wantOK    bool
		wantIn    []string
	}{
		{"half the CPU idle spawns", 0.9, 48.6, true, nil},
		{"idle exactly at the threshold spawns", 0.9, 10, true, nil},
		{"under the idle the threshold needs holds and names the reading", 0.9, 4.2, false, []string{"4.2% CPU idle", "0.96", "0.90"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := pressureDaemon(t, tt.threshold, tt.idle, nil)
			got := d.checkPressure("polecat")
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
			if got.CPUIdle != tt.idle {
				t.Errorf("CPUIdle = %v, want %v", got.CPUIdle, tt.idle)
			}
		})
	}
}

// A top that cannot be read is no evidence of a busy host, so the spawn goes
// ahead, but the gate says in the daemon log that it is blind: a fail-open
// gate that stays quiet looks the same as a host that is fine (gt-kc8v1).
func TestCheckPressureCPUTierUnreadableTopSpawnsAndSaysSo(t *testing.T) {
	t.Parallel()
	d, logs := pressureDaemon(t, 0.9, 0, errors.New("top did not finish within 8s: context deadline exceeded"))
	got := d.checkPressure("polecat")
	if !got.OK {
		t.Fatalf("OK = false for an unreadable top: %q", got.Reason)
	}
	if got.CPUReadErr == nil {
		t.Error("CPUReadErr = nil, want the reason top could not be read")
	}
	for _, s := range []string{"not holding the spawn", "did not finish within 8s"} {
		if !strings.Contains(logs.String(), s) {
			t.Errorf("daemon log %q does not say %q", logs.String(), s)
		}
	}
}

// A threshold tuned for the old load-per-core scale (3.0) can never trip on a
// busy share, which tops out at 1.0; the daemon says so once rather than let
// the gate go quietly dead (gt-kc8v1).
func TestCheckPressureCPUTierWarnsOnceOfAThresholdPastOne(t *testing.T) {
	t.Parallel()
	d, logs := pressureDaemon(t, 3.0, 0.5, nil)
	for range 3 {
		if got := d.checkPressure("polecat"); !got.OK {
			t.Fatalf("OK = false at a threshold of 3.0 on a 0.5%% idle host: %q", got.Reason)
		}
	}
	if n := strings.Count(logs.String(), "can never trip"); n != 1 {
		t.Errorf("warned %d times, want once; log %q", n, logs.String())
	}
	d, logs = pressureDaemon(t, 0.9, 50, nil)
	d.checkPressure("polecat")
	if strings.Contains(logs.String(), "can never trip") {
		t.Errorf("warned of a threshold of 0.9: %q", logs.String())
	}
}

// The daemon warns of a threshold past 1.0 when it starts, before any spawn
// check, and the first check does not repeat it (gt-kc8v1).
func TestWarnPressureScaleSpeaksAtStartOnce(t *testing.T) {
	t.Parallel()
	d, logs := pressureDaemon(t, 3.0, 50, nil)
	d.warnPressureScale(d.loadOperationalConfig().GetDaemonConfig().PressureCPUThresholdV())
	if !strings.Contains(logs.String(), "can never trip") {
		t.Fatalf("no warning at start: %q", logs.String())
	}
	d.checkPressure("polecat")
	if n := strings.Count(logs.String(), "can never trip"); n != 1 {
		t.Errorf("warned %d times, want once; log %q", n, logs.String())
	}
}

// With no cpuIdleFn the tier reads the idle share off top's own output, and a
// top that cannot be run is an error rather than an idle host. The top is a
// canned sample: a real read costs the host's load, not this test's time
// (gt-kc8v1, gt-fbf5n).
func TestHostCPUIdleProductionReadIsAPercent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		top     func(context.Context) ([]byte, error)
		want    float64
		wantErr bool
	}{
		{
			name: "top's last CPU line is the idle share",
			top: func(context.Context) ([]byte, error) {
				return []byte("CPU usage: 4.5% user, 0.0% sys, 95.5% idle\n"), nil
			},
			want: 95.5,
		},
		{
			name:    "a top that cannot be run is an error",
			top:     func(context.Context) ([]byte, error) { return nil, errors.New("no top here") },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newTestDaemon(t.TempDir())
			d.hostTop = tt.top
			got, err := d.hostCPUIdle()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("hostCPUIdle = %v, want an error from the unreadable top", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("hostCPUIdle failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("hostCPUIdle = %v, want the sample's %v", got, tt.want)
			}
		})
	}
}

// With no threshold configured the CPU tier is dormant: top is never run.
func TestCheckPressureCPUTierDormantWithoutThreshold(t *testing.T) {
	t.Parallel()
	d, _ := newTestDaemon(t.TempDir())
	d.cpuIdleFn = func() (float64, error) {
		t.Error("top was read with every pressure tier disabled")
		return 0, errors.New("unexpected read")
	}
	if got := d.checkPressure("polecat"); !got.OK {
		t.Fatalf("OK = false with all tiers disabled: %q", got.Reason)
	}
}

// A tmux that cannot be listed is no evidence of an empty town: the session cap
// holds the spawn and the daemon log says why, instead of counting zero
// sessions and spawning past the cap.
func TestCheckPressure_SessionCapListError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"town-settings","version":1,"operational":{"daemon":{"pressure_max_sessions":3}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d, logs := newTestDaemon(townRoot)
	tm := newFakeTmux(newFixedClock())
	tm.listErr = errors.New("error connecting to /tmp/tmux-501/gt (Connection refused)")
	d.tmux = tm

	got := d.checkPressure("polecat")
	if got.OK {
		t.Fatal("OK = true with tmux unlistable and a session cap set")
	}
	for _, s := range []string{"session cap", "Connection refused"} {
		if !strings.Contains(got.Reason, s) {
			t.Errorf("Reason %q does not name %q", got.Reason, s)
		}
	}
	if !strings.Contains(logs.String(), "Connection refused") {
		t.Errorf("daemon log %q does not say why tmux could not be listed", logs.String())
	}
}
