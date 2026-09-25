package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

func TestWispReaperInterval(t *testing.T) {
	// Default (now 1h after Dog-driven refactor)
	if got := wispReaperInterval(nil); got != defaultWispReaperInterval {
		t.Errorf("expected default %v, got %v", defaultWispReaperInterval, got)
	}

	// Custom
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:     true,
				IntervalStr: "2h",
			},
		},
	}
	if got := wispReaperInterval(config); got != 2*time.Hour {
		t.Errorf("expected 2h, got %v", got)
	}

	// Invalid falls back to default
	config.Patrols.WispReaper.IntervalStr = "nope"
	if got := wispReaperInterval(config); got != defaultWispReaperInterval {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

func TestWispReaperMaxAge(t *testing.T) {
	if got := wispReaperMaxAge(nil); got != defaultWispMaxAge {
		t.Errorf("expected default %v, got %v", defaultWispMaxAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:   true,
				MaxAgeStr: "48h",
			},
		},
	}
	if got := wispReaperMaxAge(config); got != 48*time.Hour {
		t.Errorf("expected 48h, got %v", got)
	}
}

func TestWispDeleteAge(t *testing.T) {
	if got := wispDeleteAge(nil); got != defaultWispDeleteAge {
		t.Errorf("expected default %v, got %v", defaultWispDeleteAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:      true,
				DeleteAgeStr: "336h",
			},
		},
	}
	if got := wispDeleteAge(config); got != 14*24*time.Hour {
		t.Errorf("expected 336h, got %v", got)
	}
}

func TestDefaultReaperIntervalIsOneHour(t *testing.T) {
	// Verify the default changed from 30m to 1h per issue gt-caf7.
	if defaultWispReaperInterval != 1*time.Hour {
		t.Errorf("expected default interval 1h, got %v", defaultWispReaperInterval)
	}
}

// TestDefaultStaleIssueAgeMatchesFormula is the regression guard for gt-2qzr.
// The daemon used to hardcode 7d and inject it on the dog path, overriding the
// mol-dog-reaper formula's 720h default; agent beads are idle by design and
// were 8d old, so the shorter threshold swept every agent bead in the town.
func TestDefaultStaleIssueAgeMatchesFormula(t *testing.T) {
	if defaultStaleIssueAge != 30*24*time.Hour {
		t.Fatalf("defaultStaleIssueAge = %v, want 720h to match the mol-dog-reaper formula default",
			defaultStaleIssueAge)
	}
	if got := wispReaperStaleIssueAge(nil); got != defaultStaleIssueAge {
		t.Fatalf("wispReaperStaleIssueAge(nil) = %v, want %v", got, defaultStaleIssueAge)
	}
}

func TestWispReaperStaleIssueAge(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{StaleIssueAgeStr: "45d"},
		},
	}
	// The formula renders the default as "30d"; time.ParseDuration rejects that
	// as "unknown unit d", so the day suffix has to be understood here.
	if got := wispReaperStaleIssueAge(config); got != 45*24*time.Hour {
		t.Errorf("expected 45d, got %v", got)
	}

	config.Patrols.WispReaper.StaleIssueAgeStr = "nope"
	if got := wispReaperStaleIssueAge(config); got != defaultStaleIssueAge {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

func TestParseAgeDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "720h", want: 720 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: " 7d ", want: 7 * 24 * time.Hour},
		{in: "d", wantErr: true},
		{in: "1.5d", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseAgeDuration(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseAgeDuration(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseAgeDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWispReaperAutoCloseKnob(t *testing.T) {
	yes, no := true, false

	// Unset is not disarmed: the knob exists to disarm, so absence must not
	// silently change behavior.
	if WispReaperAutoCloseDisarmed(nil) {
		t.Error("nil config should not be disarmed")
	}
	if WispReaperAutoCloseDisarmed(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}) {
		t.Error("config without a wisp_reaper section should not be disarmed")
	}

	unset := &DaemonPatrolConfig{Patrols: &PatrolsConfig{WispReaper: &WispReaperConfig{}}}
	if WispReaperAutoCloseDisarmed(unset) {
		t.Error("unset auto_close should not be disarmed")
	}
	if !wispReaperAutoCloseEnabled(unset, false) {
		t.Error("unset auto_close should leave the dog path enabled")
	}
	// The inline fallback runs because Dog dispatch FAILED. Turning a dispatch
	// error into a sweep of the durable issue tracker is the gt-2qzr failure,
	// so it needs an explicit opt-in.
	if wispReaperAutoCloseEnabled(unset, true) {
		t.Error("unset auto_close should leave the inline fallback disarmed")
	}

	armed := &DaemonPatrolConfig{Patrols: &PatrolsConfig{WispReaper: &WispReaperConfig{AutoClose: &yes}}}
	if WispReaperAutoCloseDisarmed(armed) {
		t.Error("auto_close=true should not be disarmed")
	}
	if !wispReaperAutoCloseEnabled(armed, false) || !wispReaperAutoCloseEnabled(armed, true) {
		t.Error("auto_close=true should enable both paths")
	}

	disarmed := &DaemonPatrolConfig{Patrols: &PatrolsConfig{WispReaper: &WispReaperConfig{AutoClose: &no}}}
	if !WispReaperAutoCloseDisarmed(disarmed) {
		t.Error("auto_close=false should be disarmed")
	}
	if wispReaperAutoCloseEnabled(disarmed, false) || wispReaperAutoCloseEnabled(disarmed, true) {
		t.Error("auto_close=false should disarm both paths")
	}
}

// TestDispatchReaperDogReportsFailureCause covers the diagnostics gap from
// gt-2qzr: the log recorded only "exit status 1", which said nothing about why
// the sweep had moved to the inline fallback, so the cause went unnoticed.
func TestDispatchReaperDogReportsFailureCause(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock")
	}

	binDir := t.TempDir()
	fakeGT := filepath.Join(binDir, "gt")
	script := "#!/bin/sh\necho 'sling: no available dogs in deacon/dogs' >&2\nexit 1\n"
	if err := os.WriteFile(fakeGT, []byte(script), 0755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		gtPath: fakeGT,
		logger: log.New(io.Discard, "", 0),
	}
	err := d.dispatchReaperDog(map[string]string{"max_age": "1h"})
	if err == nil {
		t.Fatal("dispatchReaperDog() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "no available dogs") {
		t.Errorf("dispatchReaperDog() error = %q, want it to carry the command's stderr", err)
	}
}

func TestDispatchReaperDogUsesDogPoolSling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock")
	}

	townRoot := t.TempDir()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "gt-args.log")
	fakeGT := filepath.Join(binDir, "gt")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", logPath)
	if err := os.WriteFile(fakeGT, []byte(script), 0755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		gtPath: fakeGT,
	}
	if err := d.dispatchReaperDog(map[string]string{"max_age": "1h"}); err != nil {
		t.Fatalf("dispatchReaperDog() error = %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gt args log: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	wantPrefix := []string{"sling", constants.MolDogReaper, "deacon/dogs"}
	if len(args) < len(wantPrefix) {
		t.Fatalf("gt args = %v, want prefix %v", args, wantPrefix)
	}
	for i, want := range wantPrefix {
		if args[i] != want {
			t.Fatalf("gt arg %d = %q, want %q (all args: %v)", i, args[i], want, args)
		}
	}
}

func TestDoltServerHostIgnoresStaleBeadsHost(t *testing.T) {
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")

	d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
	if got := d.doltServerHost(); got != "127.0.0.1" {
		t.Fatalf("doltServerHost() = %q, want default localhost", got)
	}
}

func TestDoltServerHostUsesConfiguredTownHost(t *testing.T) {
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{config: &Config{TownRoot: townRoot}}
	if got := d.doltServerHost(); got != "127.0.0.2" {
		t.Fatalf("doltServerHost() = %q, want configured host", got)
	}
}

// TestTriggerWispReaper_SkipsWhenNotDue is the regression test for
// gt-ima2/gt-gxpwc applied to wisp_reaper: with a recent last-run record on
// disk, the trigger must decline to start a cycle rather than firing on
// every tick (or every restart) regardless of the persisted schedule.
func TestTriggerWispReaper_SkipsWhenNotDue(t *testing.T) {
	townRoot := t.TempDir()
	if err := savePatrolLastRun(townRoot, "wisp_reaper", time.Now()); err != nil {
		t.Fatalf("seed last run: %v", err)
	}

	var buf strings.Builder
	d := &Daemon{
		logger: log.New(&buf, "", 0),
		config: &Config{TownRoot: townRoot},
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				WispReaper: &WispReaperConfig{Enabled: true},
			},
		},
	}

	d.triggerWispReaper()
	if d.wispReaperRunning.Load() {
		t.Error("a declined trigger must not set the running guard")
	}
	if !strings.Contains(buf.String(), "not due") {
		t.Errorf("expected a not-due log line, got: %q", buf.String())
	}
}
