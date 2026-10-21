package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/drift"
	"github.com/steveyegge/gastown/internal/land"
)

func TestJevDriftPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_drift") {
		t.Error("jev_drift must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_drift") {
		t.Error("jev_drift must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_drift") {
		t.Error("an explicit enabled=false must keep jev_drift off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_drift") {
		t.Error("an explicit enabled=true must turn jev_drift on")
	}
}

// With the patrol off the trigger runs no pass: no single-flight guard is
// taken, no last-run record is written and nothing is logged.
func TestJevDriftTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
	}

	d.triggerJevDrift()

	if d.jevDriftRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_drift"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and names the missing price,
// without reaching Jev or git.
func TestJevDriftRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}},
		},
	}

	d.runJevDrift()

	if !strings.Contains(logs.String(), "jev_drift: error:") ||
		!strings.Contains(logs.String(), "price per billion tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

// The pass is opened with the patrol's own settings, and a failure to open it
// is logged under the patrol's name.
func TestJevDriftRunOpensThePassWithItsSettings(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	var got drift.Settings
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
		jevDriftOpen: func(_ string, s drift.Settings, _ io.Writer) (*drift.Pass, error) {
			got = s
			return nil, errors.New("boom")
		},
	}

	d.runJevDrift()

	if got.MaxUSDPerDay != 2.5 || got.USDPerBillionTokens != 3 {
		t.Errorf("settings = %+v, want the 2.5 cap at 3 USD per billion tokens", got)
	}
	if !strings.Contains(logs.String(), "jev_drift: error: boom") {
		t.Errorf("log = %q, want the open failure under jev_drift", logs)
	}
}

func TestJevDriftConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_drift":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevDrift
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_drift = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// With the patrol on and no last run on record, the trigger starts one pass on
// its own goroutine, the pass runs to its end, and the last-run time is saved.
func TestJevDriftTriggerRunsThePassWhenOn(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	var opened atomic.Bool
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}},
		},
		jevDriftOpen: func(string, drift.Settings, io.Writer) (*drift.Pass, error) {
			opened.Store(true)
			return nil, errors.New("boom")
		},
	}

	d.triggerJevDrift()
	// The guard is taken before the goroutine starts and released last, after
	// the last-run time is saved, so a released guard means the pass is over.
	for d.jevDriftRunning.Load() {
		time.Sleep(time.Millisecond)
	}

	if !opened.Load() {
		t.Fatal("the due pass was never opened")
	}
	if !strings.Contains(logs.String(), "jev_drift: error: boom") {
		t.Errorf("log = %q, want the open failure under jev_drift", logs)
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_drift"); !found {
		t.Error("no last-run time was saved after the pass")
	}
}

// A pass already in flight blocks the trigger: nothing is opened or logged.
func TestJevDriftTriggerSingleFlight(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	var opened atomic.Bool
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}},
		},
		jevDriftOpen: func(string, drift.Settings, io.Writer) (*drift.Pass, error) {
			opened.Store(true)
			return nil, nil
		},
	}
	d.jevDriftRunning.Store(true)

	d.triggerJevDrift()

	if opened.Load() {
		t.Error("a second pass was opened while one was in flight")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged while a pass was in flight: %s", logs)
	}
}

// A pass that finds another pass holding the run lock logs that it skipped and
// asks nothing.
func TestJevDriftRunLogsABusyLockAsSkipped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	release, err := battery.Lock(drift.CursorPath(town) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}},
		},
		jevDriftOpen: func(string, drift.Settings, io.Writer) (*drift.Pass, error) {
			return &drift.Pass{CursorPath: drift.CursorPath(town)}, nil
		},
	}

	d.runJevDrift()

	if !strings.Contains(logs.String(), "jev_drift: skipped: another jev drift pass is running") {
		t.Errorf("log = %q, want the busy pass logged as skipped", logs)
	}
}

// A pass that skips a landing logs its summary: the landings asked, the ones
// skipped and whether the cap was reached.
func TestJevDriftRunLogsTheSummary(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := battery.WriteCursor(drift.CursorPath(town), battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}},
		},
		jevDriftOpen: func(string, drift.Settings, io.Writer) (*drift.Pass, error) {
			return &drift.Pass{
				Landings:   jevDriftLandings{{BeadID: "gt-refused", Rig: drift.RigName, Head: "h", Base: "b", LandedAt: t0.Add(time.Minute)}},
				Git:        jevDriftRefusingRepo{},
				LogPath:    drift.LogPath(town),
				CursorPath: drift.CursorPath(town),
				Allows:     func() (bool, error) { return true, nil },
			}, nil
		},
	}

	d.runJevDrift()

	if want := "jev_drift: asked 0 landings, skipped 1 (daily cap reached: false)"; !strings.Contains(logs.String(), want) {
		t.Errorf("log = %q, want %q", logs, want)
	}
}

// jevDriftLandings lists the records it holds, whatever cursor it is asked from.
type jevDriftLandings []land.LandingRecord

func (f jevDriftLandings) Since(time.Time) ([]land.LandingRecord, error) {
	return f, nil
}

// jevDriftRefusingRepo syncs fine and refuses every landing's refs, so each landing is
// skipped with no line.
type jevDriftRefusingRepo struct {
	drift.Repo
}

func (jevDriftRefusingRepo) Sync(context.Context) error { return nil }

func (jevDriftRefusingRepo) Numstat(_ context.Context, _, head string) (string, error) {
	return "", fmt.Errorf("%w: %q", drift.ErrBadRef, head)
}
