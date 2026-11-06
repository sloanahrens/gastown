package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// jevScorecardTestZone fixes the zone the configured hour is read in, so a
// test's hour does not depend on the host's time zone.
var jevScorecardTestZone = time.FixedZone("jev-scorecard-test", -8*60*60)

// jevScorecardTestRunner is a refresh runner that starts no process: it counts
// the runs, answers each with err, and runs onSuccess after a run that
// succeeded, as the real command rewriting the cache would.
type jevScorecardTestRunner struct {
	calls     int
	err       error
	onSuccess func()
}

func (r *jevScorecardTestRunner) run(context.Context, string) error {
	r.calls++
	if r.err == nil && r.onSuccess != nil {
		r.onSuccess()
	}
	return r.err
}

// jevScorecardTestCache writes the town's scorecard cache scored at scoredAt.
func jevScorecardTestCache(t *testing.T, town string, scoredAt time.Time) {
	t.Helper()
	doc := scorecard.CacheDoc{
		ScoredAt:  scoredAt,
		Since:     "7d",
		Batteries: []scorecard.BatteryBlock{{Name: scorecard.AttentionBattery, ScoredAt: scoredAt}},
	}
	if err := (scorecard.Cache{}).Write(scorecard.CachePath(town), doc); err != nil {
		t.Fatalf("writing the scorecard cache: %v", err)
	}
}

// jevScorecardTestDaemon is a daemon with what the patrol reads: the clock,
// the town root, the patrol config and its seams. The refresh runs inline, so
// a test reads its result as soon as the trigger returns.
func jevScorecardTestDaemon(t *testing.T, now time.Time, cfg *JevScorecardConfig, runner *jevScorecardTestRunner, logs *bytes.Buffer) (*Daemon, *clockwork.FakeClock) {
	t.Helper()
	town := t.TempDir()
	clk := clockwork.NewFakeClockAt(now)
	d := &Daemon{
		config:       &Config{TownRoot: town},
		logger:       log.New(logs, "", 0),
		clock:        clk,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevScorecard: cfg}},
		jevScorecardSeams: jevScorecardSeams{
			loc:      func() *time.Location { return jevScorecardTestZone },
			refresh:  runner.run,
			dispatch: func(fn func()) { fn() },
		},
	}
	return d, clk
}

// jevScorecardEnabled is the patrol's block with the battery the tests score.
func jevScorecardEnabled(batteries ...string) *JevScorecardConfig {
	return &JevScorecardConfig{Enabled: true, Batteries: batteries}
}

// TestJevScorecardPatrolIsOptIn holds the patrol off until the town asks for
// it, and holds the trigger to that: with the block absent or disabled it runs
// nothing and records no attempt.
func TestJevScorecardPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_scorecard") {
		t.Error("jev_scorecard must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_scorecard") {
		t.Error("jev_scorecard must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JevScorecard: &JevScorecardConfig{Enabled: false, Batteries: []string{scorecard.AttentionBattery}},
	}}
	if IsPatrolEnabled(off, "jev_scorecard") {
		t.Error("an explicit enabled=false must keep jev_scorecard off")
	}

	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, jevScorecardTestZone)
	d, _ := jevScorecardTestDaemon(t, now, off.Patrols.JevScorecard, runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, now.Add(-48*time.Hour))

	d.triggerJevScorecard()

	if runner.calls != 0 {
		t.Errorf("refreshes with the patrol off = %d, want 0", runner.calls)
	}
	if _, found, err := loadPatrolLastRun(d.config.TownRoot, "jev_scorecard"); err != nil || found {
		t.Errorf("last-run record with the patrol off: found=%v err=%v, want none", found, err)
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// TestJevScorecardPatrolIsALifecycleHeartbeatStep holds the wiring: the
// heartbeat runs the patrol, and it is lifecycle, so an E-stopped town starts
// no refresh.
func TestJevScorecardPatrolIsALifecycleHeartbeatStep(t *testing.T) {
	t.Parallel()
	for _, s := range heartbeatSteps {
		if s.name == "jev-scorecard" {
			if !s.lifecycle {
				t.Error("jev-scorecard must be a lifecycle step")
			}
			return
		}
	}
	t.Error("no jev-scorecard heartbeat step")
}

// TestJevScorecardPatrolConfigDecodes reads the patrol's keys from daemon.json
// at load time.
func TestJevScorecardPatrolConfigDecodes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_scorecard":{` +
		`"enabled":true,"hour":"03:15","interval":"12h","batteries":["attention"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevScorecard
	if c == nil || !c.Enabled || c.Hour != "03:15" || c.IntervalStr != "12h" {
		t.Fatalf("jev_scorecard = %+v, want enabled at 03:15 every 12h", c)
	}
	if got := c.BatteriesValue(); len(got) != 1 || got[0] != scorecard.AttentionBattery {
		t.Errorf("batteries = %v, want [%s]", got, scorecard.AttentionBattery)
	}
}

// TestJevScorecardPatrolRunsAtTheHourOnceADay holds the daily clock: with a
// last attempt recorded, the refresh runs at the configured hour once and not
// again until the next day's hour.
func TestJevScorecardPatrolRunsAtTheHourOnceADay(t *testing.T) {
	t.Parallel()
	monday := time.Date(2026, 11, 2, 0, 0, 0, 0, jevScorecardTestZone)
	lastRun := monday.Add(2 * time.Hour) // a refresh at 02:00, so the cache is fresh at the hour
	start := monday.Add(3 * time.Hour)
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	d, clk := jevScorecardTestDaemon(t, start, jevScorecardEnabled(scorecard.AttentionBattery), runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, lastRun)
	runner.onSuccess = func() { jevScorecardTestCache(t, d.config.TownRoot, clk.Now()) }
	if err := savePatrolLastRun(d.config.TownRoot, "jev_scorecard", lastRun); err != nil {
		t.Fatal(err)
	}

	d.triggerJevScorecard()
	if runner.calls != 0 {
		t.Fatalf("refreshes at start on a cache within the interval = %d, want 0", runner.calls)
	}

	clk.Advance(1*time.Hour + 29*time.Minute) // Monday 04:29
	d.triggerJevScorecard()
	if runner.calls != 0 {
		t.Fatalf("refreshes at 04:29 = %d, want 0", runner.calls)
	}

	clk.Advance(time.Minute) // Monday 04:30
	d.triggerJevScorecard()
	if runner.calls != 1 {
		t.Fatalf("refreshes at the hour = %d, want 1", runner.calls)
	}

	clk.Advance(7*time.Hour + 30*time.Minute) // Monday 12:00
	d.triggerJevScorecard()
	if runner.calls != 1 {
		t.Fatalf("refreshes again the same day = %d, want 1", runner.calls)
	}

	clk.Advance(16*time.Hour + 30*time.Minute) // Tuesday 04:30
	d.triggerJevScorecard()
	if runner.calls != 2 {
		t.Fatalf("refreshes the next day at the hour = %d, want 2", runner.calls)
	}

	clk.Advance(7*time.Hour + 30*time.Minute) // Tuesday 12:00
	d.triggerJevScorecard()
	if runner.calls != 2 {
		t.Errorf("refreshes twice on the next day = %d, want 2", runner.calls)
	}
}

// TestJevScorecardPatrolAtStartLeavesAFreshCacheAlone holds the start rule: a
// town with no recorded attempt and a cache within the interval waits for the
// next hour instead of refreshing as the daemon comes up.
func TestJevScorecardPatrolAtStartLeavesAFreshCacheAlone(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, jevScorecardTestZone)
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	d, clk := jevScorecardTestDaemon(t, start, jevScorecardEnabled(scorecard.AttentionBattery), runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, start.Add(-23*time.Hour))

	d.triggerJevScorecard()
	if runner.calls != 0 {
		t.Fatalf("refreshes at start on a cache 23h old = %d, want 0", runner.calls)
	}
	clk.Advance(30 * time.Minute) // Monday 10:30, still within the interval and before the next hour
	d.triggerJevScorecard()
	if runner.calls != 0 {
		t.Errorf("refreshes before the next hour = %d, want 0", runner.calls)
	}
}

// TestJevScorecardPatrolAtStartIgnoresAHourThatPassedWhileDown holds the start
// rule when a record predates this daemon: an hour that passed while the
// daemon was down does not refresh a cache the interval still covers.
func TestJevScorecardPatrolAtStartIgnoresAHourThatPassedWhileDown(t *testing.T) {
	t.Parallel()
	sunday := time.Date(2026, 11, 1, 0, 0, 0, 0, jevScorecardTestZone)
	lastRun := sunday.Add(4*time.Hour + 30*time.Minute) // Sunday 04:30
	start := sunday.Add(24*time.Hour + 10*time.Hour)    // Monday 10:00
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	d, _ := jevScorecardTestDaemon(t, start, jevScorecardEnabled(scorecard.AttentionBattery), runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, start.Add(-time.Hour)) // a fresh cache from 09:00
	if err := savePatrolLastRun(d.config.TownRoot, "jev_scorecard", lastRun); err != nil {
		t.Fatal(err)
	}

	d.triggerJevScorecard()

	if runner.calls != 0 {
		t.Errorf("refreshes at start past an hour that passed while down = %d, want 0", runner.calls)
	}
}

// TestJevScorecardPatrolAtStartRefreshesAStaleCache holds the other half of
// the start rule: a cache older than the interval is refreshed at once.
func TestJevScorecardPatrolAtStartRefreshesAStaleCache(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, jevScorecardTestZone)
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	d, _ := jevScorecardTestDaemon(t, start, jevScorecardEnabled(scorecard.AttentionBattery), runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, start.Add(-25*time.Hour))

	d.triggerJevScorecard()

	if runner.calls != 1 {
		t.Fatalf("refreshes at start on a cache 25h old = %d, want 1", runner.calls)
	}
	if !strings.Contains(logs.String(), "refreshed the scorecard cache") {
		t.Errorf("log = %q, want the refresh reported", logs)
	}
}

// TestJevScorecardPatrolRefreshesWhenTheCacheIsMissing covers an absent cache:
// an age nobody knows is not a fresh one, so the start rule runs.
func TestJevScorecardPatrolRefreshesWhenTheCacheIsMissing(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, jevScorecardTestZone)
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	d, _ := jevScorecardTestDaemon(t, start, jevScorecardEnabled(scorecard.AttentionBattery), runner, logs)

	d.triggerJevScorecard()

	if runner.calls != 1 {
		t.Errorf("refreshes with no cache = %d, want 1", runner.calls)
	}
}

// TestJevScorecardPatrolSkipsATickWhileARefreshRuns holds the single-flight
// rule: a tick that finds a refresh in flight starts nothing, and says so once
// an hour rather than once a heartbeat.
func TestJevScorecardPatrolSkipsATickWhileARefreshRuns(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, jevScorecardTestZone)
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	d, clk := jevScorecardTestDaemon(t, start, jevScorecardEnabled(scorecard.AttentionBattery), runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, start.Add(-25*time.Hour))
	d.jevScorecardRunning.Store(true) // a refresh that started on an earlier tick

	d.triggerJevScorecard()
	if runner.calls != 0 {
		t.Fatalf("started a second refresh = %d, want 0", runner.calls)
	}
	if got := strings.Count(logs.String(), "still running"); got != 1 {
		t.Fatalf("skip lines = %d, want 1; log = %q", got, logs)
	}

	clk.Advance(10 * time.Minute)
	d.triggerJevScorecard()
	if got := strings.Count(logs.String(), "still running"); got != 1 {
		t.Errorf("skip lines ten minutes later = %d, want still 1; log = %q", got, logs)
	}

	clk.Advance(50 * time.Minute)
	d.triggerJevScorecard()
	if got := strings.Count(logs.String(), "still running"); got != 2 {
		t.Errorf("skip lines an hour later = %d, want 2; log = %q", got, logs)
	}
}

// TestJevScorecardPatrolAFailedRefreshIsLoggedOnce holds the failure rule: a
// refresh that exits non-zero is one line, is not retried on the next
// heartbeat, and the next scheduled run still happens.
func TestJevScorecardPatrolAFailedRefreshIsLoggedOnce(t *testing.T) {
	t.Parallel()
	monday := time.Date(2026, 11, 2, 0, 0, 0, 0, jevScorecardTestZone)
	start := monday.Add(3 * time.Hour)
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{err: errors.New("scoring failed for attention")}
	d, clk := jevScorecardTestDaemon(t, start, jevScorecardEnabled(scorecard.AttentionBattery), runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, start.Add(-25*time.Hour))

	d.triggerJevScorecard()
	if runner.calls != 1 {
		t.Fatalf("refreshes at start on a stale cache = %d, want 1", runner.calls)
	}
	if got := strings.Count(logs.String(), "refresh failed"); got != 1 {
		t.Fatalf("failure lines = %d, want 1; log = %q", got, logs)
	}

	clk.Advance(10 * time.Minute)
	d.triggerJevScorecard()
	if runner.calls != 1 {
		t.Errorf("retried on the next tick = %d, want 1", runner.calls)
	}
	if got := strings.Count(logs.String(), "refresh failed"); got != 1 {
		t.Errorf("failure lines on the next tick = %d, want 1; log = %q", got, logs)
	}

	clk.Advance(80 * time.Minute) // Monday 04:30, the day's hour
	d.triggerJevScorecard()
	if runner.calls != 2 {
		t.Fatalf("refreshes at the hour after a failure = %d, want 2", runner.calls)
	}

	clk.Advance(24 * time.Hour) // Tuesday 04:30
	d.triggerJevScorecard()
	if runner.calls != 3 {
		t.Errorf("refreshes the next day after a failure = %d, want 3", runner.calls)
	}
}

// TestJevScorecardPatrolEmptyBatteriesRefreshNothing holds the empty list: a
// town that declares no battery has nothing to score, so the patrol starts no
// refresh even with a stale cache.
func TestJevScorecardPatrolEmptyBatteriesRefreshNothing(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, jevScorecardTestZone)
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	d, _ := jevScorecardTestDaemon(t, start, jevScorecardEnabled(), runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, start.Add(-25*time.Hour))

	d.triggerJevScorecard()

	if runner.calls != 0 {
		t.Errorf("refreshes with an empty battery list = %d, want 0", runner.calls)
	}
}

// TestJevScorecardPatrolUnknownBatteryIsRefused holds the name check: a
// battery the binary does not register is an error that names it when the
// patrol reads its settings, and the trigger reports it instead of refreshing
// as if the battery cost nothing.
func TestJevScorecardPatrolUnknownBatteryIsRefused(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, jevScorecardTestZone)
	logs := &bytes.Buffer{}
	runner := &jevScorecardTestRunner{}
	d, _ := jevScorecardTestDaemon(t, start, jevScorecardEnabled("attention", "retired_battery"), runner, logs)
	jevScorecardTestCache(t, d.config.TownRoot, start.Add(-25*time.Hour))

	if _, err := jevScorecardSettingsOf(d.patrolConfig); err == nil || !strings.Contains(err.Error(), `"retired_battery"`) {
		t.Fatalf("settings error = %v, want one naming retired_battery", err)
	}

	d.triggerJevScorecard()
	if runner.calls != 0 {
		t.Errorf("refreshes past an unknown battery = %d, want 0", runner.calls)
	}
	if !strings.Contains(logs.String(), `"retired_battery"`) {
		t.Errorf("log = %q, want the unknown battery named", logs)
	}
}

// TestJevScorecardPatrolSettingsRefuseBadValues drives the settings errors for
// a config built in code: decoding refuses these from a file, so this is the
// other way a bad value reaches the patrol.
func TestJevScorecardPatrolSettingsRefuseBadValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  *JevScorecardConfig
		want string
	}{
		{"hour not a clock", &JevScorecardConfig{Hour: "4am"}, "jev_scorecard.hour"},
		{"interval not a duration", &JevScorecardConfig{IntervalStr: "daily"}, "jev_scorecard.interval"},
	} {
		_, err := jevScorecardSettingsOf(&DaemonPatrolConfig{Patrols: &PatrolsConfig{JevScorecard: tc.cfg}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: settings error = %v, want one naming %s", tc.name, err, tc.want)
		}
	}
}

// TestJevScorecardPatrolRefreshNeedsTheGTBinary holds the runner's refusal for
// a daemon that resolved no binary: it names the missing binary, and starts
// nothing.
func TestJevScorecardPatrolRefreshNeedsTheGTBinary(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}}

	err := d.runJevScorecardRefresh(context.Background(), d.config.TownRoot)

	if err == nil || !strings.Contains(err.Error(), "no gt binary resolved") {
		t.Errorf("refresh error = %v, want the missing gt binary named", err)
	}
}

// TestJevScorecardPatrolRefreshReportsTheExit holds the runner's failure path:
// the exec seam answers with an error and no process starts, so the error the
// patrol logs names the command and carries no output.
func TestJevScorecardPatrolRefreshReportsTheExit(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		gtPath: "/nonexistent/gt",
		execCmd: func(*exec.Cmd) ([]byte, []byte, error) {
			return nil, []byte("captured output that must stay out of the log"), errors.New("exit status 1")
		},
	}

	err := d.runJevScorecardRefresh(context.Background(), d.config.TownRoot)

	if err == nil || !strings.Contains(err.Error(), "gt jev scorecard --refresh") {
		t.Fatalf("refresh error = %v, want the command named", err)
	}
	if strings.Contains(err.Error(), "captured output") {
		t.Errorf("refresh error = %v, want no captured output", err)
	}
}

// TestJevScorecardPatrolDefaults holds the settings a town leaves out: the
// configured defaults are what the patrol runs on.
func TestJevScorecardPatrolDefaults(t *testing.T) {
	t.Parallel()
	var c *JevScorecardConfig // a town that names no block at all
	clockMinute, err := c.ClockMinute()
	if err != nil || clockMinute != 4*60+30 {
		t.Errorf("default clock minute = %d (err %v), want 04:30", clockMinute, err)
	}
	interval, err := c.IntervalDuration()
	if err != nil || interval != 24*time.Hour {
		t.Errorf("default interval = %v (err %v), want 24h", interval, err)
	}
	if got := c.BatteriesValue(); got != nil {
		t.Errorf("default batteries = %v, want none", got)
	}
}
