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
