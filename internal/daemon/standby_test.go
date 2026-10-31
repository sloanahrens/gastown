package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// standbyKey is the key the Ring is minted and checked under in these tests.
// The town's settings name it as ${GT_RING_KEY}, which resolves through the
// daemon's getenv seam (withStandbyRingKey), so no test reads the process
// environment.
var standbyKey = []byte("standby-ring-key")

// standbyTestNow is the fixed hour these cases run on: the Ring record and the
// daemon's clock both read it, so they do not shift with the wall clock.
var standbyTestNow = time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)

// withStandbyRingKey writes a town settings file that names the Ring key and
// gives d the environment that resolves it.
func withStandbyRingKey(t *testing.T, d *Daemon) {
	t.Helper()
	path := config.TownSettingsPath(d.config.TownRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"type":"town-settings","version":1,"ring":{"key":"${GT_RING_KEY}"}}`
	if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	d.getenv = func(name string) string {
		if name == "GT_RING_KEY" {
			return string(standbyKey)
		}
		return ""
	}
}

// standbyCondition is one way the town stands by, put in place on the
// daemon's town at now.
type standbyCondition struct {
	name string
	set  func(t *testing.T, d *Daemon, now time.Time)
}

// standbyConditions are the three holds the standby sites that take the hold
// file read: a Waned Ring, the operator's hold file and the town ESTOP.
var standbyConditions = []standbyCondition{
	{"waned Ring", func(t *testing.T, d *Daemon, now time.Time) {
		withStandbyRingKey(t, d)
		ringtest.Waned(t, d.config.TownRoot, standbyKey, now)
	}},
	{"hold file", func(t *testing.T, d *Daemon, _ time.Time) {
		writeOperatorHold(t, d.config.TownRoot)
	}},
	{"town ESTOP", func(t *testing.T, d *Daemon, _ time.Time) {
		if err := estop.Activate(d.config.TownRoot, estop.TriggerManual, "drill"); err != nil {
			t.Fatal(err)
		}
	}},
}

// standbyHeartbeat is a daemon whose heartbeat records the steps it would run
// instead of running them, on a clock fixed at now. The lines it logs are
// returned too.
func standbyHeartbeat(t *testing.T, now time.Time) (*Daemon, *[]string, func() []string) {
	t.Helper()
	d := upgradeTestDaemon(t)
	d.clock = clockwork.NewFakeClockAt(now)
	logger, lines := lineLogger()
	d.logger = logger
	var ran []string
	d.seams.heartbeatStep = func(_ *Daemon, step heartbeatStep) { ran = append(ran, step.name) }
	return d, &ran, lines
}

// standbyStepNames lists the heartbeat steps, the lifecycle ones or the rest,
// in the order the heartbeat runs them.
func standbyStepNames(lifecycle bool) []string {
	var names []string
	for _, s := range heartbeatSteps {
		if s.lifecycle == lifecycle {
			names = append(names, s.name)
		}
	}
	return names
}

// A Waned Ring and the town ESTOP skip the lifecycle steps and run the rest; a
// Valid Ring, a Ring-less town and the hold file alone run every step. The
// hold file is the drain's own brake and must not stop the heartbeat's
// lifecycle steps (gt-6mlik.6).
func TestStandbyHeartbeat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		set       func(t *testing.T, d *Daemon)
		lifecycle bool
	}{
		{"waned Ring skips the lifecycle steps", func(t *testing.T, d *Daemon) {
			withStandbyRingKey(t, d)
			ringtest.Waned(t, d.config.TownRoot, standbyKey, now)
		}, false},
		{"town ESTOP skips the lifecycle steps", func(t *testing.T, d *Daemon) {
			if err := estop.Activate(d.config.TownRoot, estop.TriggerManual, "drill"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"valid Ring runs every step", func(t *testing.T, d *Daemon) {
			withStandbyRingKey(t, d)
			ringtest.Valid(t, d.config.TownRoot, standbyKey, now)
		}, true},
		{"no ring section runs every step", func(t *testing.T, d *Daemon) {
			withStandbyRingKey(t, d)
		}, true},
		{"hold file alone runs every step", func(t *testing.T, d *Daemon) {
			writeOperatorHold(t, d.config.TownRoot)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, ran, _ := standbyHeartbeat(t, now)
			tc.set(t, d)

			d.heartbeat(&State{})

			want := allHeartbeatStepNames()
			if !tc.lifecycle {
				want = standbyStepNames(false)
			}
			if !slices.Equal(*ran, want) {
				t.Errorf("steps run = %v, want %v", *ran, want)
			}
		})
	}
}

// allHeartbeatStepNames lists every heartbeat step in the order it runs.
func allHeartbeatStepNames() []string {
	names := make([]string, 0, len(heartbeatSteps))
	for _, s := range heartbeatSteps {
		names = append(names, s.name)
	}
	return names
}

// The standby is logged once when it appears and once when it lifts, over a
// Ring that wanes and is then renewed.
func TestStandbyHeartbeatLogsOnceAsTheRingWanesAndRenews(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	d, _, lines := standbyHeartbeat(t, now)
	withStandbyRingKey(t, d)

	ringtest.Waned(t, d.config.TownRoot, standbyKey, now)
	for i := 0; i < 3; i++ {
		d.heartbeat(&State{})
	}
	if n := countContaining(lines(), "heartbeat: standing by", "the Ring waned at"); n != 1 {
		t.Errorf("waned Ring logged %d times over 3 ticks, want once; log: %v", n, lines())
	}

	ringtest.Valid(t, d.config.TownRoot, standbyKey, now)
	for i := 0; i < 3; i++ {
		d.heartbeat(&State{})
	}
	if n := countContaining(lines(), "heartbeat: standby lifted"); n != 1 {
		t.Errorf("standby lift logged %d times over 3 ticks, want once; log: %v", n, lines())
	}
}

// The tier sweep site logs its standby once when it appears and once when it
// lifts, over a Ring that wanes and is then renewed (gt-6mlik.6).
func TestStandbyTierSweepLogsOnceAsTheRingWanesAndRenews(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	tierCycleStarts(d)
	withStandbyRingKey(t, d)

	ringtest.Waned(t, d.config.TownRoot, standbyKey, now)
	for i := 0; i < 3; i++ {
		d.triggerTierSweep()
	}
	if n := strings.Count(logs.String(), "tier_sweep: standing by"); n != 1 {
		t.Errorf("waned Ring logged %d times over 3 triggers, want once; log: %s", n, logs.String())
	}

	ringtest.Valid(t, d.config.TownRoot, standbyKey, now)
	for i := 0; i < 3; i++ {
		d.triggerTierSweep()
	}
	waitTierSweepIdle(t, d)
	if n := strings.Count(logs.String(), "tier_sweep: standby lifted"); n != 1 {
		t.Errorf("standby lift logged %d times over 3 triggers, want once; log: %s", n, logs.String())
	}
}

// The scheduled maintenance site logs its standby once when it appears and
// once when it lifts, over an operator hold written and then removed. The
// ticks keep reaching the site because a town with no databases to back up
// leaves the window unconsumed.
func TestStandbyMaintenanceLogsOnceAsTheHoldAppearsAndLifts(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	withGCFakes(t, d)
	d.patrolConfig = gcModeConfig()
	d.maint.now = func() time.Time { return gcTestNow }

	writeOperatorHold(t, d.config.TownRoot)
	for i := 0; i < 3; i++ {
		d.runScheduledMaintenance()
	}
	if n := strings.Count(logs.String(), "scheduled_maintenance: standing by"); n != 1 {
		t.Errorf("hold file logged %d times over 3 ticks, want once; log: %s", n, logs.String())
	}

	if err := os.Remove(filepath.Join(d.config.TownRoot, "seat-refill.hold")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		d.runScheduledMaintenance()
	}
	if n := strings.Count(logs.String(), "scheduled_maintenance: standby lifted"); n != 1 {
		t.Errorf("standby lift logged %d times over 3 ticks, want once; log: %s", n, logs.String())
	}
}

// A standby backup whose database discovery fails says so: the backup does
// not run, and the error is logged rather than dropped.
func TestStandbyBackupLogsDiscoveryFailure(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	d.patrolConfig = gcModeConfig()
	d.maint.now = func() time.Time { return gcTestNow }
	d.maint.gcDatabases = func(string) ([]string, error) { return nil, errors.New("dolt is not answering") }
	writeOperatorHold(t, d.config.TownRoot)

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 0 {
		t.Errorf("a backup ran with no databases discovered: %v", f.backupCalls)
	}
	if n := countContaining(strings.Split(strings.TrimSpace(logs.String()), "\n"), "standby backup", "dolt is not answering"); n != 1 {
		t.Errorf("the discovery failure was logged %d times, want once; log: %s", n, logs.String())
	}
}

// tierCycleStarts makes d's cycle record that it started: the first seam a
// cycle reads origin/main through (on both the due and the early path) counts
// the call, and the worktree seam is stubbed so no real checkout is taken.
func tierCycleStarts(d *Daemon) *int {
	started := new(int)
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
		*started++
		return "abc123", nil
	}
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	return started
}

// Under each standby the tier sweep starts no cycle and creates no bead; with
// none of them a due cycle starts.
func TestStandbyTierSweep(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	for _, c := range standbyConditions {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d, rec, bd, _ := newTierSweepDaemon(t, now, "gastown")
			rec.green()
			started := tierCycleStarts(d)
			c.set(t, d, now)

			d.triggerTierSweep()
			waitTierSweepIdle(t, d)

			if *started != 0 {
				t.Errorf("a cycle started under %s: origin/main read %d time(s)", c.name, *started)
			}
			if len(rec.stages) != 0 {
				t.Errorf("stages ran under %s: %+v", c.name, rec.stages)
			}
			if len(bd.created) != 0 {
				t.Errorf("beads created under %s: %+v", c.name, bd.created)
			}
		})
	}

	t.Run("no standby starts a due cycle", func(t *testing.T) {
		t.Parallel()
		d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
		rec.green()
		started := tierCycleStarts(d)

		d.triggerTierSweep()
		waitTierSweepIdle(t, d)

		if *started == 0 {
			t.Fatal("a due cycle with no standby did not start")
		}
	})
}

// Under each standby neither the window gc nor the catch-up gc starts, the
// catch-up backup still runs, and a gc already running is not interrupted.
func TestStandbyMaintenanceGC(t *testing.T) {
	t.Parallel()
	for _, c := range standbyConditions {
		t.Run("window gc under "+c.name, func(t *testing.T) {
			t.Parallel()
			d, _ := gcTestDaemon(t)
			f := withGCFakes(t, d)
			f.sizes = map[string]gcMeasure{"gt": {633 * mib, 0}, "hq": {631 * mib, 0}}
			d.patrolConfig = gcModeConfig()
			d.maint.now = func() time.Time { return gcTestNow }
			c.set(t, d, gcTestNow)

			d.runScheduledMaintenance()

			if len(f.gcCalls) != 0 {
				t.Errorf("window gc ran under %s: %v", c.name, f.gcCalls)
			}
			if len(f.backupCalls) != 2 {
				t.Errorf("window backup under %s = %v, want gt and hq", c.name, f.backupCalls)
			}
			// The window is consumed by its backup: the next tick takes no second one.
			d.runScheduledMaintenance()
			if len(f.backupCalls) != 2 {
				t.Errorf("second tick in the window backed up again under %s: %v", c.name, f.backupCalls)
			}
		})

		t.Run("catch-up gc under "+c.name, func(t *testing.T) {
			t.Parallel()
			d, f := catchUpGCDaemon(t)
			catchUpGCStale(t, d, "gt")
			c.set(t, d, catchUpNow)

			d.runScheduledMaintenance()

			if len(f.gcCalls) != 0 {
				t.Errorf("catch-up gc ran under %s: %v", c.name, f.gcCalls)
			}
		})

		t.Run("catch-up backup under "+c.name, func(t *testing.T) {
			t.Parallel()
			d, f := catchUpDaemon(t)
			c.set(t, d, catchUpNow)

			d.runScheduledMaintenance()

			if len(f.backupCalls) != 2 {
				t.Errorf("catch-up backup calls under %s = %v, want gt and hq", c.name, f.backupCalls)
			}
			if len(f.gcCalls) != 0 {
				t.Errorf("gc ran under %s beside the backup: %v", c.name, f.gcCalls)
			}
		})

		t.Run("running gc is not interrupted under "+c.name, func(t *testing.T) {
			t.Parallel()
			d, _ := gcTestDaemon(t)
			f := withGCFakes(t, d)
			f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
			d.patrolConfig = gcModeConfig()
			d.maint.now = func() time.Time { return gcTestNow }
			c.set(t, d, gcTestNow)
			d.maintenanceGCRunning.Store(true)

			d.runScheduledMaintenance()

			if !d.maintenanceGCRunning.Load() {
				t.Errorf("the running gc was cleared under %s", c.name)
			}
			if len(f.gcCalls) != 0 {
				t.Errorf("a second gc started under %s: %v", c.name, f.gcCalls)
			}
		})
	}

	// The control: with no standby the same catch-up gc is due and runs, so
	// the empty gcCalls above is the standby's doing.
	t.Run("catch-up gc runs with no standby", func(t *testing.T) {
		t.Parallel()
		d, f := catchUpGCDaemon(t)
		catchUpGCStale(t, d, "gt")

		d.runScheduledMaintenance()

		if len(f.gcCalls) == 0 {
			t.Fatal("catch-up gc did not run with no standby")
		}
	})
}

// The lifecycle flags are pinned to today's set, the Dolt step and the
// landing worker read no standby check, and a standby set by the hold file
// does not reach the heartbeat.
func TestStandbyExceptions(t *testing.T) {
	t.Parallel()
	wantLifecycle := []string{
		"ghost-sessions", "plugins", "polecat-health", "idle-reap", "orphan-processes",
		"dispatch", "rebuild-gt", "rebuild-om", "jev-dedupe", "jev-attention",
		"jev-landing", "jev-submission", "jev-drift", "jev-logtriage", "jev-rework",
		"jev-dispatch", "jev-thrash", "jev-rules", "jev-conflict", "jev-progress",
		"jev-stale", "jev-notes",
	}
	if got := standbyStepNames(true); !slices.Equal(got, wantLifecycle) {
		t.Errorf("lifecycle steps = %v, want %v", got, wantLifecycle)
	}

	t.Run("dolt supervision runs under a waned Ring", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
		d, ran, _ := standbyHeartbeat(t, now)
		withStandbyRingKey(t, d)
		ringtest.Waned(t, d.config.TownRoot, standbyKey, now)

		d.heartbeat(&State{})

		if !slices.Contains(*ran, "dolt") {
			t.Errorf("dolt supervision did not run under a waned Ring: %v", *ran)
		}
	})

	t.Run("landing worker passes run under a waned Ring", func(t *testing.T) {
		t.Parallel()
		now := standbyTestNow
		d, _ := landingDrainDaemon(t)
		d.clock = clockwork.NewFakeClockAt(now)
		withStandbyRingKey(t, d)
		ringtest.Waned(t, d.config.TownRoot, standbyKey, now)
		var passes atomic.Int32
		pass := func(context.Context) landworker.Report {
			passes.Add(1)
			return landworker.Report{}
		}

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		// The loop stops when d.ctx is canceled, so the test owns that context
		// and joins the goroutine it started instead of leaving it running past
		// the test (the hour interval is abandoned with the context).
		loopCtx, stopLoop := context.WithCancel(d.ctx)
		d.ctx = loopCtx
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			d.landingWorkerLoop("gastown", time.Hour, pass)
		}()
		defer func() {
			stopLoop()
			select {
			case <-loopDone:
			case <-ctx.Done():
				t.Error("the landing worker loop did not stop after its context was canceled")
			}
		}()

		for passes.Load() == 0 {
			if ctx.Err() != nil {
				t.Fatal("the landing worker ran no pass under a waned Ring")
			}
			runtime.Gosched()
		}
	})

	t.Run("the hold file is not a heartbeat standby", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
		d, ran, _ := standbyHeartbeat(t, now)
		writeOperatorHold(t, d.config.TownRoot)

		d.heartbeat(&State{})

		if !slices.Equal(*ran, allHeartbeatStepNames()) {
			t.Errorf("the hold file held heartbeat steps: %v", *ran)
		}
	})

	t.Run("the hold file is the tier sweep and gc standby", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		writeOperatorHold(t, d.config.TownRoot)
		if reason := d.standbyReason(gcTestNow, true); !strings.Contains(reason, "operator dispatch hold present") {
			t.Errorf("standbyReason(true) = %q under the hold file, want the operator hold", reason)
		}
		if reason := d.standbyReason(gcTestNow, false); reason != "" {
			t.Errorf("standbyReason(false) = %q under the hold file, want none", reason)
		}
	})
}
