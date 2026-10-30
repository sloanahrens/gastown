package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
)

func TestJevLogtriagePatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_logtriage") {
		t.Error("jev_logtriage must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_logtriage") {
		t.Error("jev_logtriage must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_logtriage") {
		t.Error("an explicit enabled=false must keep jev_logtriage off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_logtriage") {
		t.Error("an explicit enabled=true must turn jev_logtriage on")
	}
}

// With the patrol off the trigger runs no pass: no single-flight guard is
// taken, no last-run record is written and nothing is logged.
func TestJevLogtriageTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
	}

	d.triggerJevLogtriage()

	if d.jevLogtriageRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_logtriage"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and names the missing price,
// without reading the daemon log or reaching Jev.
func TestJevLogtriageRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{Enabled: true}},
		},
	}

	d.runJevLogtriage()

	if !strings.Contains(logs.String(), "jev_logtriage: error:") ||
		!strings.Contains(logs.String(), "usd_per_billion_tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

// The pass is opened with the patrol's own settings, and a failure to open it
// is logged under the patrol's name.
func TestJevLogtriageRunOpensThePassWithItsSettings(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	var got logtriage.Settings
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
		jevLogtriageOpen: func(_ string, s logtriage.Settings, _ io.Writer) (*logtriage.Pass, error) {
			got = s
			return nil, errors.New("boom")
		},
	}

	d.runJevLogtriage()

	if got.MaxUSDPerDay != 2.5 || got.USDPerBillionTokens != 3 {
		t.Errorf("settings = %+v, want the 2.5 cap at 3 USD per billion tokens", got)
	}
	if !strings.Contains(logs.String(), "jev_logtriage: error: boom") {
		t.Errorf("log = %q, want the open failure under jev_logtriage", logs)
	}
}

func TestJevLogtriageConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_logtriage":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevLogtriage
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_logtriage = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// With the patrol on and no last run on record, the trigger starts one pass on
// its own goroutine, the pass runs to its end, and the last-run time is saved.
func TestJevLogtriageTriggerRunsThePassWhenOn(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	var opened atomic.Bool
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{Enabled: true}},
		},
		jevLogtriageOpen: func(string, logtriage.Settings, io.Writer) (*logtriage.Pass, error) {
			opened.Store(true)
			return nil, errors.New("boom")
		},
	}

	d.triggerJevLogtriage()
	waitJevLogtriageIdle(t, d)

	if !opened.Load() {
		t.Fatal("the due pass was never opened")
	}
	if !strings.Contains(logs.String(), "jev_logtriage: error: boom") {
		t.Errorf("log = %q, want the open failure under jev_logtriage", logs)
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_logtriage"); !found {
		t.Error("no last-run time was saved after the pass")
	}
}

// A pass already in flight blocks the trigger: nothing is opened or logged.
func TestJevLogtriageTriggerSingleFlight(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	var opened atomic.Bool
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{Enabled: true}},
		},
		jevLogtriageOpen: func(string, logtriage.Settings, io.Writer) (*logtriage.Pass, error) {
			opened.Store(true)
			return nil, nil
		},
	}
	d.jevLogtriageRunning.Store(true)

	d.triggerJevLogtriage()

	if opened.Load() {
		t.Error("a second pass opened while one was in flight")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged while a pass was in flight: %s", logs)
	}
}

// The pass runs every fifteen minutes, and its budget is bounded inside that
// interval so a slow pass cannot run into the next one.
func TestJevLogtriageIntervalIsFifteenMinutes(t *testing.T) {
	t.Parallel()
	if jevLogtriageInterval != 15*time.Minute {
		t.Errorf("jevLogtriageInterval = %v, want 15 minutes", jevLogtriageInterval)
	}
	if jevLogtriageRunBudget <= 0 || jevLogtriageRunBudget >= jevLogtriageInterval {
		t.Errorf("run budget = %v, want a positive budget under the interval", jevLogtriageRunBudget)
	}
}

// waitJevLogtriageIdle waits for the single-flight guard to be released. The
// guard is released last, after the last-run time is saved, so a released guard
// means the pass is over.
func waitJevLogtriageIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevLogtriageRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_logtriage pass did not finish")
		}
		runtime.Gosched()
	}
}

// TestJevLogtriagePassStopsBetweenCallsWhenEstopped covers gt-dlnkz: the E-stop
// kept a pass from starting, but one already in flight went on paying for every
// group it reached. The call in flight finishes and is recorded; the groups
// behind it are carried to a pass run after the E-stop lifts.
func TestJevLogtriagePassStopsBetweenCallsWhenEstopped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	daemonLog := logtriage.DaemonLogPath(town)
	if err := os.MkdirAll(filepath.Dir(daemonLog), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := "2026/10/21 03:31:35 spec_dispatch: tick: 57 candidate(s), 2 refused, 0 planning\n" +
		"2026/10/21 03:32:01 landing_worker: gt-rpdhw: merge failed at 77b9028a after 3s\n" +
		"2026/10/21 03:34:00 kernel: WARNING: disk nearly full\n"
	if err := os.WriteFile(daemonLog, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	asker := &estoppingAsker{t: t, town: town}
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{Enabled: true}},
		},
		jevLogtriageOpen: func(string, logtriage.Settings, io.Writer) (*logtriage.Pass, error) {
			return &logtriage.Pass{
				Asker:      asker,
				DaemonLog:  daemonLog,
				LogPath:    logtriage.LogPath(town),
				CursorPath: logtriage.CursorPath(town),
				Allows:     func() (bool, error) { return true, nil },
			}, nil
		},
	}

	d.runJevLogtriage()

	if asker.calls != 1 {
		t.Errorf("Jev was asked about %d groups, want only the one in flight when the E-stop went up", asker.calls)
	}
	if !strings.Contains(logs.String(), "jev_logtriage: stopped: E-STOP active") {
		t.Errorf("log = %q, want the pass to say it stopped for the E-stop", logs)
	}
}

// A pass logs the repeats it skipped beside the groups it asked, through the
// pass's own run: the summary line is the only place the repeats are visible.
func TestJevLogtriageSummaryLogsTheRepeats(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	daemonLog := logtriage.DaemonLogPath(town)
	if err := os.MkdirAll(filepath.Dir(daemonLog), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := "2026/10/21 03:34:00 kernel: WARNING: disk nearly full\n"
	if err := os.WriteFile(daemonLog, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	// The pass's own check and the group's allow the ask; the first repeat
	// finds the day's spend used up, so every repeat is skipped.
	allowed := []bool{true, true, false}
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{Enabled: true}},
		},
		jevLogtriageOpen: func(string, logtriage.Settings, io.Writer) (*logtriage.Pass, error) {
			return &logtriage.Pass{
				Asker:      batteryAsker{},
				DaemonLog:  daemonLog,
				LogPath:    logtriage.LogPath(town),
				CursorPath: logtriage.CursorPath(town),
				Allows: func() (bool, error) {
					ok := allowed[0]
					if len(allowed) > 1 {
						allowed = allowed[1:]
					}
					return ok, nil
				},
				Sampled: func(string, int) bool { return true },
			}, nil
		},
	}

	d.runJevLogtriage()

	if !strings.Contains(logs.String(), "asked 1 groups, 0 unanswered, 0 repeated, 5 repeats skipped") {
		t.Errorf("log = %q, want the skipped repeats in the pass's line", logs)
	}
}

// A count the pass kept always earns a line, and a pass that did nothing
// leaves none.
func TestJevLogtriageSummaryNamesEveryCount(t *testing.T) {
	t.Parallel()
	if line := jevLogtriageSummary(logtriage.Summary{}); line != "" {
		t.Errorf("line = %q, want no line from a pass that did nothing", line)
	}
	for _, tc := range []struct {
		name string
		sum  logtriage.Summary
	}{
		{"asked groups", logtriage.Summary{Asked: 1}},
		{"unanswered groups", logtriage.Summary{Unanswered: 1}},
		{"deferred groups", logtriage.Summary{Deferred: 1}},
		{"dropped groups", logtriage.Summary{Dropped: 1}},
		{"repeats logged", logtriage.Summary{Repeated: 1}},
		{"repeats skipped", logtriage.Summary{RepeatsSkipped: 1}},
		{"the daily cap", logtriage.Summary{CapReached: true}},
	} {
		if line := jevLogtriageSummary(tc.sum); line == "" {
			t.Errorf("a pass that only counted %s logged nothing", tc.name)
		}
	}
}
