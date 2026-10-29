package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os/exec"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// hmSyncNow is a fixed instant: every case that does not move the clock reads
// the same time, so an interval comparison is the only thing under test.
var hmSyncNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// hmSyncCall is one sync the patrol ran: the program and its arguments.
type hmSyncCall struct {
	name string
	args []string
}

// hmSyncFakes are the patrol's injected clock, runner and the state the test
// reads back.
type hmSyncFakes struct {
	now time.Time
	// advance moves the clock forward while the sync runs, so the summary
	// line's duration is a value the test chose.
	advance time.Duration
	calls   []hmSyncCall
	// err is what the next run returns.
	err error
}

// hmSyncTestDaemon builds a daemon whose patrol runs inline, on the fake
// clock, with the patrol's config as given.
func hmSyncTestDaemon(t *testing.T, cfg *DaemonPatrolConfig) (*Daemon, *bytes.Buffer, *hmSyncFakes) {
	t.Helper()
	f := &hmSyncFakes{now: hmSyncNow}
	var buf bytes.Buffer
	d := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(&buf, "", 0),
		patrolConfig: cfg,
	}
	d.hmSync.now = func() time.Time { return f.now }
	d.hmSync.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == hmSyncStatusArg && args[1] == hmSyncStatusJSON {
			// The health check reads the status through the same seam. These
			// tests are about the sync, so the read answers as a fresh town and
			// the alert stays quiet (hm_sync_alert_test.go).
			return []byte(hmSyncHealthFresh), nil
		}
		f.calls = append(f.calls, hmSyncCall{name: name, args: args})
		f.now = f.now.Add(f.advance)
		return nil, f.err
	}
	// The alert starts known closed, so the fresh read above clears nothing and
	// logs nothing. Alerts go to no-op functions rather than the notifier: the
	// unit tier starts no gt, and these tests assert on the sync.
	d.hmSyncAlerts.mark(hmSyncAlertClosed)
	d.hmSyncAlerts.raise = func(string, string, string) error { return nil }
	d.hmSyncAlerts.clearFn = func(string, ...string) error { return nil }
	// A sync runs inline: a test asserts what it did, so it must have done it
	// by the time runHMSync returns.
	d.hmSync.dispatch = func(fn func()) { fn() }
	return d, &buf, f
}

// hmSyncConfig is an hm_sync block with the given fields; empty ones are left
// for the patrol's own defaults.
func hmSyncConfig(enabled bool, interval, command string) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{
			HMSync: &HMSyncConfig{Enabled: enabled, Interval: interval, Command: command},
		},
	}
}

// TestHMSyncDisabledByDefault: the patrol is off unless the config says
// enabled, whether the block is absent, present-but-off, or there is no config
// at all.
func TestHMSyncDisabledByDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config *DaemonPatrolConfig
	}{
		{"no config", nil},
		{"no patrols section", &DaemonPatrolConfig{Type: "daemon-patrol-config", Version: 1}},
		{"no hm_sync block", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}},
		{"block present and off", hmSyncConfig(false, "1m", "hm")},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, f := hmSyncTestDaemon(t, tt.config)

			d.runHMSync()

			if len(f.calls) != 0 {
				t.Errorf("ran %v, want no sync: the patrol is off", f.calls)
			}
			if logs.Len() != 0 {
				t.Errorf("logged %q, want silence from a disabled patrol", logs.String())
			}
		})
	}
}

// TestHMSyncShippedDefaultDoesNotEnableIt: the default config a town gets from
// an install declares the key with the design's interval and leaves the patrol
// off — the live config gets the key, never the behaviour.
func TestHMSyncShippedDefaultDoesNotEnableIt(t *testing.T) {
	t.Parallel()

	cfg := DefaultLifecycleConfig()
	hc := hmSyncSettings(cfg)
	if hc == nil {
		t.Fatal("the shipped default config does not declare patrols.hm_sync")
	}
	if hc.Enabled {
		t.Error("the shipped default enables hm_sync: an install must not start running hm on a timer")
	}
	if hc.Interval != agentconfig.HMSyncDefaultInterval {
		t.Errorf("interval = %q, want %q", hc.Interval, agentconfig.HMSyncDefaultInterval)
	}
	if hc.Command != agentconfig.HMSyncDefaultCommand {
		t.Errorf("command = %q, want %q", hc.Command, agentconfig.HMSyncDefaultCommand)
	}
	if IsPatrolEnabled(cfg, "hm_sync") {
		t.Error("IsPatrolEnabled says the shipped default runs the patrol")
	}
	if IsPatrolEnabled(nil, "hm_sync") {
		t.Error("IsPatrolEnabled(nil) says the patrol runs: an opt-in patrol with no config is off")
	}
}

// TestHMSyncRunsOncePerInterval: the first tick runs the configured program,
// a tick inside the interval does not, and the tick after it does.
func TestHMSyncRunsOncePerInterval(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "hm"))

	d.runHMSync()
	if len(f.calls) != 1 {
		t.Fatalf("ran %d syncs on the first tick, want 1", len(f.calls))
	}
	if got := f.calls[0]; got.name != "hm" || strings.Join(got.args, " ") != "sync" {
		t.Errorf("ran %q %v, want hm sync", got.name, got.args)
	}

	f.now = f.now.Add(29 * time.Minute)
	d.runHMSync()
	if len(f.calls) != 1 {
		t.Fatalf("ran %d syncs 29m into a 30m interval, want 1", len(f.calls))
	}

	f.now = f.now.Add(time.Minute)
	d.runHMSync()
	if len(f.calls) != 2 {
		t.Fatalf("ran %d syncs at the end of the interval, want 2", len(f.calls))
	}
	if n := strings.Count(logs.String(), "hm_sync: "); n != 2 {
		t.Errorf("logged %d summary lines for 2 syncs: %q", n, logs.String())
	}
}

// TestHMSyncRunsTheConfiguredCommand: the command is the town's, and sync is
// its one argument.
func TestHMSyncRunsTheConfiguredCommand(t *testing.T) {
	t.Parallel()
	d, _, f := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "/opt/hm/bin/hm"))

	d.runHMSync()

	if len(f.calls) != 1 {
		t.Fatalf("ran %d syncs, want 1", len(f.calls))
	}
	if got := f.calls[0]; got.name != "/opt/hm/bin/hm" || strings.Join(got.args, " ") != "sync" {
		t.Errorf("ran %q %v, want /opt/hm/bin/hm sync", got.name, got.args)
	}
}

// TestHMSyncDefaultsItsIntervalAndCommand: a town that sets only enabled gets
// a 30m interval and the hm command, and the schema's default interval is the
// one the patrol runs on.
func TestHMSyncDefaultsItsIntervalAndCommand(t *testing.T) {
	t.Parallel()

	def, err := time.ParseDuration(agentconfig.HMSyncDefaultInterval)
	if err != nil {
		t.Fatalf("config.HMSyncDefaultInterval %q is not a duration: %v", agentconfig.HMSyncDefaultInterval, err)
	}
	if def != hmSyncDefaultInterval {
		t.Fatalf("config.HMSyncDefaultInterval = %v, the patrol runs on %v: the two defaults disagree",
			def, hmSyncDefaultInterval)
	}

	d, _, f := hmSyncTestDaemon(t, hmSyncConfig(true, "", ""))
	if got, err := hmSyncInterval(d.patrolConfig); err != nil || got != hmSyncDefaultInterval {
		t.Errorf("interval = %v, %v, want the default %v and no error", got, err, hmSyncDefaultInterval)
	}
	if got := hmSyncCommand(d.patrolConfig); got != "hm" {
		t.Errorf("command = %q, want hm", got)
	}

	d.runHMSync()
	if len(f.calls) != 1 || f.calls[0].name != "hm" {
		t.Fatalf("ran %v, want one hm sync", f.calls)
	}
}

// TestHMSyncTickerStartsOnTheRunInterval: a setting the patrol can schedule
// starts a check ticker some fraction of the run interval long, and the line it
// logs says both.
func TestHMSyncTickerStartsOnTheRunInterval(t *testing.T) {
	t.Parallel()
	d, logs, _ := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "hm"))

	ticker := d.startHMSyncTicker()
	if ticker == nil {
		t.Fatal("no ticker for a 30m interval")
	}
	ticker.Stop()

	if got, want := logs.String(), "HM sync ticker started (check every 5m0s, run interval 30m0s)\n"; got != want {
		t.Errorf("logged %q, want %q", got, want)
	}
}

// TestHMSyncTickerRefusedForAnIntervalItCannotSchedule: the config file's bad
// interval is refused at decode, so this is the code-built one. It starts no
// ticker and the one line it logs names the value, rather than a patrol that
// never runs and never says why.
func TestHMSyncTickerRefusedForAnIntervalItCannotSchedule(t *testing.T) {
	t.Parallel()
	d, logs, _ := hmSyncTestDaemon(t, hmSyncConfig(true, "half an hour", "hm"))

	if ticker := d.startHMSyncTicker(); ticker != nil {
		ticker.Stop()
		t.Fatal("started a ticker for an interval the patrol cannot schedule")
	}
	got := logs.String()
	if !strings.Contains(got, "not started") || !strings.Contains(got, `"half an hour"`) {
		t.Errorf("logged %q, want one line naming the refused interval", got)
	}
}

// TestHMSyncInvalidIntervalIsRefused: a config built in code with an interval
// the patrol cannot schedule runs nothing and logs why, rather than reading the
// bad value as the default and syncing on a cadence the town never asked for.
// The config file's own refusal is the decoder's, driven in internal/config.
func TestHMSyncInvalidIntervalIsRefused(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, interval string }{
		{"unparseable", "half an hour"},
		{"zero", "0s"},
		{"negative", "-5m"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, f := hmSyncTestDaemon(t, hmSyncConfig(true, tt.interval, "hm"))

			d.runHMSync()
			if len(f.calls) != 0 {
				t.Fatalf("ran %v, want no sync: %q is not an interval the patrol can schedule", f.calls, tt.interval)
			}
			got := logs.String()
			if !strings.Contains(got, "hm_sync: not running") || !strings.Contains(got, tt.interval) {
				t.Errorf("logged %q, want a refusal naming %q", got, tt.interval)
			}

			// A whole default interval later the setting is still refused: no
			// tick may fall back to the default the town did not ask for.
			f.now = f.now.Add(hmSyncDefaultInterval)
			d.runHMSync()
			if len(f.calls) != 0 {
				t.Fatalf("ran %v a default interval later, want no sync", f.calls)
			}
		})
	}
}

// TestHMSyncFailureIsLoggedAndRetried: a failing sync gets its own summary
// line with the exit status, and the next interval runs it again rather than
// leaving the patrol dead.
func TestHMSyncFailureIsLoggedAndRetried(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "hm"))
	f.err = exitErr(3)
	f.advance = 2 * time.Second

	d.runHMSync()
	if len(f.calls) != 1 {
		t.Fatalf("ran %d syncs, want 1", len(f.calls))
	}
	if got := logs.String(); !strings.Contains(got, "hm_sync: hm sync: exit 3 in 2s") {
		t.Errorf("logged %q, want one summary line naming exit 3 and the duration", got)
	}

	d.runHMSync()
	if len(f.calls) != 1 {
		t.Fatalf("ran %d syncs inside the interval after a failure, want 1", len(f.calls))
	}

	// The failed sync counts as this interval's run: the patrol comes back on
	// the next one, not on every check tick.
	f.now = f.now.Add(30 * time.Minute)
	d.runHMSync()
	if len(f.calls) != 2 {
		t.Fatalf("ran %d syncs after the interval passed, want 2: a failure must not stop the patrol", len(f.calls))
	}
	if n := strings.Count(logs.String(), "hm_sync: "); n != 2 {
		t.Errorf("logged %d summary lines for 2 syncs: %q", n, logs.String())
	}
}

// TestHMSyncSuccessSummaryLine: a successful sync logs one line with exit 0
// and how long it took.
func TestHMSyncSuccessSummaryLine(t *testing.T) {
	t.Parallel()
	d, logs, f := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "hm"))
	f.advance = 90 * time.Second

	d.runHMSync()

	if got, want := logs.String(), "hm_sync: hm sync: exit 0 in 1m30s\n"; got != want {
		t.Errorf("logged %q, want %q", got, want)
	}
}

// TestHMSyncDoesNotRunTwiceConcurrently: a tick that lands while a sync is
// still running is skipped, even when the interval has passed.
func TestHMSyncDoesNotRunTwiceConcurrently(t *testing.T) {
	t.Parallel()
	d, _, f := hmSyncTestDaemon(t, hmSyncConfig(true, "1m", "hm"))
	// While the first sync runs, the interval passes and the loop ticks again.
	// The second tick must find the first still in flight and skip.
	inFlight := false
	d.hmSync.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == hmSyncStatusArg && args[1] == hmSyncStatusJSON {
			return []byte(hmSyncHealthFresh), nil
		}
		f.calls = append(f.calls, hmSyncCall{name: name, args: args})
		if !inFlight {
			inFlight = true
			f.now = f.now.Add(2 * time.Minute)
			d.runHMSync()
		}
		return nil, nil
	}

	d.runHMSync()

	if len(f.calls) != 1 {
		t.Fatalf("ran %d syncs, want 1: a tick while a sync is in flight must be skipped", len(f.calls))
	}
	if d.hmSyncRunning.Load() {
		t.Error("the single-flight guard is still set after the sync returned")
	}
}

// TestHMSyncProductionRunSeam: with no injected runner the patrol runs the
// command through the daemon's exec seam, and a failed run's error names the
// program without echoing its output.
func TestHMSyncProductionRunSeam(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		d, logs, _ := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "hm"))
		d.hmSync.run = nil
		var argv []string
		d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
			if len(cmd.Args) > 1 && cmd.Args[1] == "sync" {
				argv = cmd.Args // the sync; the health check's status read follows it
			}
			return []byte("indexed 3 sources\n"), nil, nil
		}

		d.runHMSync()

		if strings.Join(argv, " ") != "hm sync" {
			t.Errorf("ran %v, want hm sync", argv)
		}
		if got := logs.String(); !strings.Contains(got, "hm_sync: hm sync: exit 0") {
			t.Errorf("logged %q, want the success summary line", got)
		}
	})

	t.Run("failure", func(t *testing.T) {
		t.Parallel()
		d, logs, _ := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "hm"))
		d.hmSync.run = nil
		d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) {
			return nil, []byte("cannot open /home/operator/.hm/index: permission denied\n"), exitErr(4)
		}

		d.runHMSync()

		got := logs.String()
		if !strings.Contains(got, "exit 4") {
			t.Errorf("logged %q, want the exit status", got)
		}
		if strings.Contains(got, "permission denied") {
			t.Errorf("logged %q: the command's output must not reach the log through the error path", got)
		}
	})

	// A program that could not start has no exit status at all, and the
	// summary line must still name what happened rather than print an empty
	// status.
	t.Run("program missing", func(t *testing.T) {
		t.Parallel()
		d, logs, _ := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "hm"))
		d.hmSync.run = nil
		d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) {
			return nil, nil, errors.New("exec: \"hm\": executable file not found in $PATH")
		}

		d.runHMSync()

		if got := logs.String(); !strings.Contains(got, "no exit status") {
			t.Errorf("logged %q, want a summary line saying there was no exit status", got)
		}
	})

	// A sync the run timeout killed, or a shutdown stopped, never exits on its
	// own: ExitCode reports -1 for it, and the summary line must say the sync
	// was killed rather than print a status of -1.
	t.Run("killed", func(t *testing.T) {
		t.Parallel()
		d, logs, _ := hmSyncTestDaemon(t, hmSyncConfig(true, "30m", "hm"))
		d.hmSync.run = nil
		d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) {
			return nil, nil, exitErr(-1)
		}

		d.runHMSync()

		got := logs.String()
		if !strings.Contains(got, "hm sync: killed in ") {
			t.Errorf("logged %q, want a summary line saying the sync was killed", got)
		}
		if strings.Contains(got, "exit -1") {
			t.Errorf("logged %q: a killed sync has no exit status to report as -1", got)
		}
	})
}

// TestHMSyncConfigDecodes: the block decodes as written, and an interval or
// key the patrol cannot act on is refused at decode rather than read as a
// default. The decoder lives in internal/config, so its refusals are driven
// there (TestHMSyncConfigRefusesWhatThePatrolCannotActOn); this reads the
// daemon's alias of the same type.
func TestHMSyncConfigDecodes(t *testing.T) {
	t.Parallel()

	var cfg PatrolsConfig
	design := `{"hm_sync": {"enabled": true, "interval": "15m", "command": "hm"}}`
	if err := json.Unmarshal([]byte(design), &cfg); err != nil {
		t.Fatalf("the block does not decode: %v", err)
	}
	hc := cfg.HMSync
	if hc == nil {
		t.Fatal("hm_sync decoded as absent")
	}
	if !hc.Enabled || hc.Interval != "15m" || hc.Command != "hm" {
		t.Errorf("decoded %+v, want the block as written", hc)
	}
}
