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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
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
	waitJevDriftIdle(t, d)

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

// waitJevDriftIdle waits for the single-flight guard to be released. The guard
// is released last, after the last-run time is saved, so a released guard
// means the pass is over.
func waitJevDriftIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevDriftRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_drift pass did not finish")
		}
		runtime.Gosched()
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

// jevDriftDocsRepo serves one diff and one two-section doc for any landing, so
// a landing is asked about two sections.
type jevDriftDocsRepo struct {
	drift.Repo
}

const jevDriftTwoSectionDoc = "# Doc\n\n## First\n\nRead it with `gt jev landing --recent N`.\n\n## Second\n\nThe `--recent` flag also limits the list.\n"

const jevDriftRenamePatch = "--- a/internal/cmd/jev_landing.go\n+++ b/internal/cmd/jev_landing.go\n@@ -12 +12 @@\n-Read it with `gt jev landing --recent N`.\n+Read it with `gt jev landing --last N`.\n"

func (jevDriftDocsRepo) Sync(context.Context) error { return nil }

func (jevDriftDocsRepo) Numstat(context.Context, string, string) (string, error) {
	return "1\t1\tinternal/cmd/jev_landing.go\n", nil
}

func (jevDriftDocsRepo) Patch(context.Context, string, string) (string, error) {
	return jevDriftRenamePatch, nil
}

func (jevDriftDocsRepo) ListDocs(context.Context, string) ([]string, error) {
	return []string{"docs/reference.md"}, nil
}

func (jevDriftDocsRepo) ReadDoc(context.Context, string, string) (string, error) {
	return jevDriftTwoSectionDoc, nil
}

// jevDriftMixedRepo serves the docs landing's diff and refuses every other
// landing's refs, so a pass both skips a landing and asks about one.
type jevDriftMixedRepo struct {
	jevDriftDocsRepo
}

func (jevDriftMixedRepo) Numstat(_ context.Context, _, head string) (string, error) {
	if head != "h" {
		return "", fmt.Errorf("%w: %q", drift.ErrBadRef, head)
	}
	return "1\t1\tinternal/cmd/jev_landing.go\n", nil
}

func (jevDriftMixedRepo) Patch(_ context.Context, _, head string) (string, error) {
	if head != "h" {
		return "", fmt.Errorf("%w: %q", drift.ErrBadRef, head)
	}
	return jevDriftRenamePatch, nil
}

// jevDriftCountingAsker counts the asks and passes each to the next asker, so
// the control case measures the same fixture without an E-stop.
type jevDriftCountingAsker struct {
	inner jevAsker
	calls int
}

func (a *jevDriftCountingAsker) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	return a.inner.Ask(ctx, state, qs)
}

// TestJevDriftPassStopsBetweenCallsWhenEstopped covers gt-dlnkz: the E-stop
// kept a pass from starting, but one already in flight went on paying for every
// doc section it reached. The call in flight finishes; the section behind it
// waits for a pass run after the E-stop lifts. The control case raises no
// E-stop, so the one call in the E-stop case is the stop's doing and not the
// fixture's (gt-l40jb).
func TestJevDriftPassStopsBetweenCallsWhenEstopped(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	landings := jevDriftLandings{
		{BeadID: "gt-bad", Rig: drift.RigName, Head: "bad", Base: "b", LandedAt: t0.Add(time.Minute)},
		{BeadID: "gt-a", Rig: drift.RigName, Head: "h", Base: "b", LandedAt: t0.Add(2 * time.Minute)},
	}
	for _, tc := range []struct {
		name      string
		asker     func(t *testing.T, town string) (jevAsker, func() int)
		wantCalls int
		wantLog   string
	}{
		{
			name: "E-stop raised in the first call",
			asker: func(t *testing.T, town string) (jevAsker, func() int) {
				a := &estoppingAsker{t: t, town: town}
				return a, func() int { return a.calls }
			},
			wantCalls: 1,
			wantLog:   "jev_drift: stopped: E-STOP active after asking 0 landing(s), skipped 1",
		},
		{
			name: "control, no E-stop",
			asker: func(*testing.T, string) (jevAsker, func() int) {
				a := &jevDriftCountingAsker{inner: batteryAsker{}}
				return a, func() int { return a.calls }
			},
			wantCalls: 2,
			wantLog:   "jev_drift: asked 1 landings, skipped 1 (daily cap reached: false)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if err := battery.WriteCursor(drift.CursorPath(town), battery.Cursor{Time: t0}); err != nil {
				t.Fatal(err)
			}
			asker, calls := tc.asker(t, town)
			logs := &bytes.Buffer{}
			d := &Daemon{
				config: &Config{TownRoot: town},
				logger: log.New(logs, "", 0),
				patrolConfig: &DaemonPatrolConfig{
					Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}},
				},
				jevDriftOpen: func(string, drift.Settings, io.Writer) (*drift.Pass, error) {
					return &drift.Pass{
						Asker:      asker,
						Landings:   landings,
						Git:        jevDriftMixedRepo{},
						LogPath:    drift.LogPath(town),
						CursorPath: drift.CursorPath(town),
						Allows:     func() (bool, error) { return true, nil },
					}, nil
				},
			}

			d.runJevDrift()

			if got := calls(); got != tc.wantCalls {
				t.Errorf("Jev was asked about %d sections, want %d", got, tc.wantCalls)
			}
			if !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("log = %q, want %q", logs, tc.wantLog)
			}
		})
	}
}
