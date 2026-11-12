package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

// diskMaintNow is a quarter past four on the day of the patrol's default
// window: inside it, and the same day for every case that does not move it.
var diskMaintNow = time.Date(2026, 10, 29, 4, 45, 0, 0, time.UTC)

// diskMaintFakeTarget is a maintenance target that records what the patrol
// asked it, so a test asserts the calls rather than the registry.
type diskMaintFakeTarget struct {
	policies []diskmaint.Policy
	applied  []diskmaint.Plan
	plan     diskmaint.Plan
	planErr  error
	result   diskmaint.Result
	applyErr error
	// during, when set, runs as the plan is built: a test moves the clock through
	// a pass that takes time.
	during func()
}

func (f *diskMaintFakeTarget) Name() string { return "registry" }

func (f *diskMaintFakeTarget) Measure(context.Context) (diskmaint.Measurement, error) {
	return diskmaint.Measurement{}, nil
}

func (f *diskMaintFakeTarget) Plan(_ context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	f.policies = append(f.policies, p)
	if f.during != nil {
		f.during()
	}
	if f.planErr != nil {
		return diskmaint.Plan{}, f.planErr
	}
	plan := f.plan
	if plan.Target == "" {
		plan.Target = "registry"
	}
	return plan, nil
}

func (f *diskMaintFakeTarget) Apply(_ context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	f.applied = append(f.applied, plan)
	return f.result, f.applyErr
}

// diskMaintFakes are the patrol's injected reads and side effects.
type diskMaintFakes struct {
	target *diskMaintFakeTarget
	// extra are further targets the build produces, by name: a test gives one
	// a cadence of its own without a second daemon.
	extra      map[string]*diskMaintFakeTarget
	targets    int
	targetsErr error
	readings   []disks.Reading
	env        map[string]string
	quiet      bool
	quietWhy   string
	notify     *notifyfake.Recorder
}

// diskMaintTestDaemon builds a daemon whose patrol runs inline, on a fixed
// clock, against a fake target. The window is the default one.
func diskMaintTestDaemon(t *testing.T) (*Daemon, *bytes.Buffer, *diskMaintFakes) {
	t.Helper()
	var buf bytes.Buffer
	f := &diskMaintFakes{target: &diskMaintFakeTarget{}, quiet: true, notify: notifyfake.New()}
	d := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(&buf, "", 0),
		patrolConfig: diskMaintEnabled("", ""),
		notifier:     f.notify,
	}
	d.maint.quiet = func(*Daemon) (bool, string) { return f.quiet, f.quietWhy }
	d.diskMaint.now = func() time.Time { return diskMaintNow }
	d.diskMaint.getenv = func(key string) string { return f.env[key] }
	d.diskMaint.readDisks = func(context.Context) []disks.Reading { return f.readings }
	d.diskMaint.targets = func(context.Context, *Daemon, string) (map[string]diskmaint.Target, error) {
		f.targets++
		if f.targetsErr != nil {
			return nil, f.targetsErr
		}
		built := map[string]diskmaint.Target{"registry": f.target}
		for name, target := range f.extra {
			built[name] = target
		}
		return built, nil
	}
	// A pass runs inline: a test asserts what it did, so it must have done it
	// by the time runDiskMaintenance returns.
	d.diskMaint.dispatch = func(fn func()) { fn() }
	return d, &buf, f
}

// diskMaintEnabled is a patrol config with the patrol on, in window, with mode
// as given ("" is the dry-run default).
func diskMaintEnabled(window, mode string) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{
			DiskMaintenance: &DiskMaintenanceConfig{
				Enabled:  true,
				Interval: agentconfig.DiskMaintenanceIntervalDaily,
				Window:   window,
				Mode:     mode,
			},
		},
	}
}

// diskMaintCadence is the patrol on, in the default window, with one target
// given an interval of its own.
func diskMaintCadence(target, every string) *DaemonPatrolConfig {
	cfg := diskMaintEnabled("", "")
	cfg.Patrols.DiskMaintenance.Targets = map[string]DiskMaintenanceTarget{target: {Interval: every}}
	return cfg
}

// TestDiskMaintenanceDisabledByDefault: the patrol is off unless the config
// says enabled, whether the block is absent, present-but-off, or there is no
// config at all.
func TestDiskMaintenanceDisabledByDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config *DaemonPatrolConfig
	}{
		{"no config", nil},
		{"no patrols section", &DaemonPatrolConfig{Type: "daemon-patrol-config", Version: 1}},
		{"no disk_maintenance block", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}},
		{"block present and off", &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			DiskMaintenance: &DiskMaintenanceConfig{Enabled: false, Window: "04:30", Mode: "apply"},
		}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, f := diskMaintTestDaemon(t)
			d.patrolConfig = tt.config

			d.runDiskMaintenance()

			if f.targets != 0 {
				t.Errorf("targets built = %d, want 0: the patrol ran while disabled", f.targets)
			}
			if logs.Len() != 0 {
				t.Errorf("logged %q, want silence from a disabled patrol", logs.String())
			}
		})
	}
}

// TestDiskMaintenanceShippedDefaultDoesNotEnableIt: the default config a town
// gets from an install declares the key and leaves it off, dry, in the design's
// window — the live config gets the key, never the behaviour.
func TestDiskMaintenanceShippedDefaultDoesNotEnableIt(t *testing.T) {
	t.Parallel()
	cfg := DefaultLifecycleConfig()
	dm := diskMaintenanceSettings(cfg)
	if dm == nil {
		t.Fatal("the shipped default config does not declare patrols.disk_maintenance")
	}
	if dm.Enabled {
		t.Error("the shipped default enables disk_maintenance: an install must not prune a registry on its own")
	}
	if dm.Mode != agentconfig.DiskMaintenanceModeDryRun {
		t.Errorf("mode = %q, want %q", dm.Mode, agentconfig.DiskMaintenanceModeDryRun)
	}
	if dm.Window != "04:30" {
		t.Errorf("window = %q, want 04:30 (after the 03:00 Dolt window)", dm.Window)
	}
	if len(dm.Targets) != 0 {
		t.Errorf("targets = %+v, want none: every target keeps the daily window", dm.Targets)
	}
	// The install writes this block into a town's config and the daemon reads it
	// back through the strict decoder, so a default it refuses would poison the
	// config of every town the install touches.
	data, err := json.Marshal(map[string]any{"disk_maintenance": dm})
	if err != nil {
		t.Fatalf("marshalling the shipped default: %v", err)
	}
	var back PatrolsConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("the shipped default does not decode: %v", err)
	}
	if IsPatrolEnabled(cfg, "disk_maintenance") {
		t.Error("IsPatrolEnabled says the shipped default runs the patrol")
	}
	if IsPatrolEnabled(nil, "disk_maintenance") {
		t.Error("IsPatrolEnabled(nil) says the patrol runs: an opt-in patrol with no config is off")
	}
}

// TestDiskMaintenanceRunsOnlyInItsWindow: a tick outside the window does
// nothing, and the same tick inside it plans.
func TestDiskMaintenanceRunsOnlyInItsWindow(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("04:30", "")

	d.diskMaint.now = func() time.Time { return time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC) }
	d.runDiskMaintenance()
	if len(f.target.policies) != 0 {
		t.Fatalf("planned %d times at 02:00, want 0: 02:00 is before the 04:30 window", len(f.target.policies))
	}

	d.diskMaint.now = func() time.Time { return diskMaintNow }
	d.runDiskMaintenance()
	if len(f.target.policies) != 1 {
		t.Fatalf("planned %d times at 04:45, want 1", len(f.target.policies))
	}
}

// TestDiskMaintenanceRunsOncePerWindow: every tick inside the window after the
// first is a no-op, and the next day's window plans again.
func TestDiskMaintenanceRunsOncePerWindow(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)

	for i := 0; i < 3; i++ {
		d.runDiskMaintenance()
	}
	if len(f.target.policies) != 1 {
		t.Fatalf("planned %d times in one window, want 1", len(f.target.policies))
	}

	tomorrow := diskMaintNow.Add(24 * time.Hour)
	d.diskMaint.now = func() time.Time { return tomorrow }
	d.runDiskMaintenance()
	if len(f.target.policies) != 2 {
		t.Fatalf("planned %d times over two windows, want 2", len(f.target.policies))
	}
}

// TestDiskMaintenanceCadenceRunsOutsideTheWindow: a target with an interval
// runs on it in any window and not again until the interval has passed, while
// a target without one keeps the daily window.
func TestDiskMaintenanceCadenceRunsOutsideTheWindow(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	cadenced := &diskMaintFakeTarget{}
	f.extra = map[string]*diskMaintFakeTarget{"registry": cadenced}
	d.patrolConfig = diskMaintCadence("registry", "2h")
	now := diskMaintNow.Add(8 * time.Hour) // 12:45, well outside the 04:30 window
	d.diskMaint.now = func() time.Time { return now }

	d.runDiskMaintenance()
	if len(cadenced.policies) != 1 {
		t.Fatalf("cadenced planned %d times at 12:45, want 1: a 2h interval does not wait for the window", len(cadenced.policies))
	}

	now = now.Add(2*time.Hour - time.Minute)
	d.runDiskMaintenance()
	if len(cadenced.policies) != 1 {
		t.Fatalf("cadenced planned %d times a minute before its interval was up, want 1", len(cadenced.policies))
	}

	now = now.Add(time.Minute)
	d.runDiskMaintenance()
	if len(cadenced.policies) != 2 {
		t.Fatalf("cadenced planned %d times once its interval was up, want 2", len(cadenced.policies))
	}
}

// TestDiskMaintenanceCadenceCountsFromTheConclusion: an interval counts from when
// the target concluded its last run, not from the tick that started it, so a
// pass that takes minutes does not shorten the wait before the next one.
func TestDiskMaintenanceCadenceCountsFromTheConclusion(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	cadenced := &diskMaintFakeTarget{}
	f.extra = map[string]*diskMaintFakeTarget{"registry": cadenced}
	d.patrolConfig = diskMaintCadence("registry", "2h")
	now := diskMaintNow.Add(8 * time.Hour) // 12:45
	d.diskMaint.now = func() time.Time { return now }
	cadenced.during = func() { now = now.Add(3 * time.Minute) } // the plan takes three minutes

	d.runDiskMaintenance()
	if len(cadenced.policies) != 1 {
		t.Fatalf("cadenced planned %d times at 12:45, want 1", len(cadenced.policies))
	}

	now = diskMaintNow.Add(10*time.Hour + time.Minute) // 14:46: two hours after the tick, not the conclusion
	cadenced.during = nil
	d.runDiskMaintenance()
	if len(cadenced.policies) != 1 {
		t.Fatalf("cadenced planned %d times at 14:46, want 1: the 2h interval runs from the 12:48 conclusion, not the 12:45 tick", len(cadenced.policies))
	}

	now = diskMaintNow.Add(10*time.Hour + 3*time.Minute) // 14:48: two hours after the conclusion
	d.runDiskMaintenance()
	if len(cadenced.policies) != 2 {
		t.Fatalf("cadenced planned %d times at 14:48, want 2: its interval was up at the 12:48 conclusion", len(cadenced.policies))
	}
}

// TestDiskMaintenanceWindowSettlesAtTheConclusion: the once-a-day gap counts
// from when the pass concluded, not the tick that found it due, so a pass that
// takes minutes does not shorten the wait before the next day's pass.
func TestDiskMaintenanceWindowSettlesAtTheConclusion(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	now := diskMaintNow // 04:45, in the window
	d.diskMaint.now = func() time.Time { return now }
	f.target.during = func() { now = now.Add(3 * time.Minute) } // the plan takes three minutes

	d.runDiskMaintenance()

	if got, want := d.diskMaintenanceSettledAt(), diskMaintNow.Add(3*time.Minute); !got.Equal(want) {
		t.Errorf("window settled at %v, want %v: the daily gap counts from the pass's conclusion", got, want)
	}
}

// TestDiskMaintenanceCadenceForAnAbsentTargetIsSaidOncePerInterval: a cadence
// set for a target the build does not produce is said once and looked for per
// interval, not per tick.
func TestDiskMaintenanceCadenceForAnAbsentTargetIsSaidOncePerInterval(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.diskMaint.targets = func(context.Context, *Daemon, string) (map[string]diskmaint.Target, error) {
		return map[string]diskmaint.Target{"other": f.target}, nil
	}
	d.patrolConfig = diskMaintCadence("registry", "2h")
	now := diskMaintNow.Add(8 * time.Hour) // 12:45, outside the window
	d.diskMaint.now = func() time.Time { return now }

	d.runDiskMaintenance()

	if got := strings.Count(logs.String(), "has no such target"); got != 1 {
		t.Errorf("said the cadenced target is absent %d times, want 1: %q", got, logs.String())
	}

	now = diskMaintNow.Add(24*time.Hour + 15*time.Minute) // 05:00, inside the next window
	d.diskMaint.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		d.runDiskMaintenance()
	}
	if got := strings.Count(logs.String(), "has no such target"); got != 1 {
		t.Errorf("said the cadenced target is absent %d times, want 1: the report is not repeated per tick", got)
	}
}

// TestDiskMaintenanceCadenceSkipsWhenNotQuiet: a due interval target takes the
// same quiet gate as the daily pass, and a skipped tick does not spend the
// interval.
func TestDiskMaintenanceCadenceSkipsWhenNotQuiet(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	cadenced := &diskMaintFakeTarget{}
	f.extra = map[string]*diskMaintFakeTarget{"registry": cadenced}
	d.patrolConfig = diskMaintCadence("registry", "2h")
	d.diskMaint.now = func() time.Time { return diskMaintNow.Add(8 * time.Hour) }
	f.quiet, f.quietWhy = false, "slot held by gastown/polecats/corpus"

	d.runDiskMaintenance()

	if len(cadenced.policies) != 0 {
		t.Errorf("cadenced planned %d times while the town was busy, want 0", len(cadenced.policies))
	}
	if !strings.Contains(logs.String(), "slot held by gastown/polecats/corpus") {
		t.Errorf("log = %q, want the reason the run was skipped", logs.String())
	}

	f.quiet = true
	d.runDiskMaintenance()
	if len(cadenced.policies) != 1 {
		t.Errorf("cadenced planned %d times once the town went quiet, want 1: a skipped tick must not spend the interval", len(cadenced.policies))
	}
}

// TestDiskMaintenanceCadenceAppliesAndRetriesADeferral: a due interval target
// applies in apply mode, and an apply the plugin lock deferred leaves it due,
// so the next tick tries again rather than waiting out the interval.
func TestDiskMaintenanceCadenceAppliesAndRetriesADeferral(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	cadenced := &diskMaintFakeTarget{}
	f.extra = map[string]*diskMaintFakeTarget{"registry": cadenced}
	cfg := diskMaintCadence("registry", "2h")
	cfg.Patrols.DiskMaintenance.Mode = agentconfig.DiskMaintenanceModeApply
	d.patrolConfig = cfg
	now := diskMaintNow.Add(8 * time.Hour)
	d.diskMaint.now = func() time.Time { return now }
	cadenced.plan = diskmaint.Plan{Actions: []diskmaint.Action{{Kind: "delete-path", Ref: "ci-gocache/x"}}}
	cadenced.result = diskmaint.Result{Deferred: "polecat-image lock is held"}

	d.runDiskMaintenance()
	if len(cadenced.applied) != 1 {
		t.Fatalf("applied %d times, want 1: an interval target applies in apply mode", len(cadenced.applied))
	}
	if !strings.Contains(logs.String(), "deferred") {
		t.Errorf("log = %q, want the deferral named", logs.String())
	}

	cadenced.result = diskmaint.Result{}
	now = now.Add(5 * time.Minute)
	d.runDiskMaintenance()
	if len(cadenced.applied) != 2 {
		t.Fatalf("applied %d times, want 2: a deferral leaves the target due", len(cadenced.applied))
	}

	now = now.Add(5 * time.Minute)
	d.runDiskMaintenance()
	if len(cadenced.applied) != 2 {
		t.Fatalf("applied %d times, want 2: a completed run spends the interval", len(cadenced.applied))
	}
}

// countCacheTrims counts the trims a pass runs: each call to the seam the pass
// runs the trim through is one trim, and the trim reaches a verdict. A tick
// that only reads what the trim reads is not a trim.
func countCacheTrims(d *Daemon) *int {
	trims := new(int)
	d.diskMaint.trim = func(context.Context, string) bool {
		*trims++
		return true
	}
	return trims
}

// TestCacheTrimIntervalIsDueOutsideTheDailyWindow: the cache trim is due two
// hours after its last run in any window, and not before. A tick outside the
// window runs it when it is due, and a tick inside the window does not run it
// early.
func TestCacheTrimIntervalIsDueOutsideTheDailyWindow(t *testing.T) {
	t.Parallel()
	d, _, _ := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	now := diskMaintNow // 04:45, inside the window: the first tick finds the trim never run
	d.diskMaint.now = func() time.Time { return now }

	d.runDiskMaintenance()
	if *trims != 1 {
		t.Fatalf("trimmed %d times at 04:45, want 1: a trim that has never run is due", *trims)
	}

	now = diskMaintNow.Add(time.Hour + 59*time.Minute) // 06:44, outside the window, a minute short of 2h
	d.runDiskMaintenance()
	if *trims != 1 {
		t.Fatalf("trimmed %d times at 06:44, want 1: a minute short of its 2h interval is not due", *trims)
	}

	now = now.Add(time.Minute) // 06:45, two hours after the 04:45 run
	d.runDiskMaintenance()
	if *trims != 2 {
		t.Fatalf("trimmed %d times at 06:45, want 2: due two hours after its last run, outside the window", *trims)
	}
}

// TestCacheTrimRunsOncePerDueTick: a tick that is both in the window and due
// for the cache trim runs it once, with the window's targets planned alongside,
// and the next tick at the same moment does not run it again.
func TestCacheTrimRunsOncePerDueTick(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)

	d.runDiskMaintenance() // 04:45: in the window, and the trim is due
	if *trims != 1 {
		t.Fatalf("trimmed %d times in a tick that is both in the window and due, want 1", *trims)
	}
	if len(f.target.policies) != 1 {
		t.Fatalf("registry planned %d times, want 1: the window's pass still plans the daily targets", len(f.target.policies))
	}

	d.runDiskMaintenance()
	if *trims != 1 {
		t.Fatalf("trimmed %d times after a second tick at the same moment, want 1: the run concluded", *trims)
	}
}

// TestDiskMaintCacheTrimRunsWhileALandingPassIsInFlight: a busy town with only
// the trim due reads the disks, raises their alerts, trims, and logs no skipped
// line for it: the trim is off the quiet guard, which is why continuous landing
// passes cannot starve it (gt-gg9hv.48; docs/guides/disk-maintenance.md).
func TestDiskMaintCacheTrimRunsWhileALandingPassIsInFlight(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	reads := 0
	d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		reads++
		return []disks.Reading{{Name: "colima", UsedPct: 88}}
	}
	// Half past noon: outside the default window, so the trim is the only work
	// this tick found.
	outside := diskMaintNow.Add(8 * time.Hour)
	d.diskMaint.now = func() time.Time { return outside }
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass dv-x)"

	d.runDiskMaintenance()

	if reads != 1 {
		t.Errorf("disk reads = %d, want 1: the trim pass reads the disks", reads)
	}
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("no alert open for a disk at 88%: the trim pass must raise it")
	}
	if *trims != 1 {
		t.Errorf("trimmed %d times while a landing pass was in flight, want 1", *trims)
	}
	if strings.Contains(logs.String(), "skipped") {
		t.Errorf("log = %q, want no skipped line for a trim that ran", logs.String())
	}
}

// TestDiskMaintTargetsStillWaitForAQuietTown: with the town busy and both the
// daily targets and the trim due, the trim runs and the targets do not. They
// are said to be skipped rather than silently dropped, and they stay due for
// the next quiet tick (gt-gg9hv.48).
func TestDiskMaintTargetsStillWaitForAQuietTown(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass dv-x)"

	d.runDiskMaintenance() // 04:45, in the window: the trim and the daily target are due

	if *trims != 1 {
		t.Fatalf("trimmed %d times while a landing pass was in flight, want 1", *trims)
	}
	if len(f.target.policies) != 0 {
		t.Fatalf("planned the target %d times while the town was busy, want 0", len(f.target.policies))
	}
	if !strings.Contains(logs.String(), "targets skipped") ||
		!strings.Contains(logs.String(), "landing pass dv-x") {
		t.Errorf("log = %q, want the targets' skip named with the reason", logs.String())
	}

	// Five minutes on, the town is quiet: the target the busy tick left is due,
	// and the trim that concluded keeps its interval.
	quiet := diskMaintNow.Add(5 * time.Minute)
	d.diskMaint.now = func() time.Time { return quiet }
	f.quiet, f.quietWhy = true, ""
	d.runDiskMaintenance()

	if got := len(f.target.policies); got != 1 {
		t.Errorf("planned the target %d times once the town went quiet, want 1: a skipped target stays due", got)
	}
	if *trims != 1 {
		t.Errorf("trimmed %d times once the town went quiet, want 1: the trim keeps its interval", *trims)
	}
}

// diskMaintAlertOnly is a patrol on a busy town with nothing due: the clock is
// outside the daily window and the cache trim's interval is spent, so the only
// work a tick can find is the alert-only pass. now is the test's clock.
type diskMaintAlertOnly struct {
	d          *Daemon
	logs       *bytes.Buffer
	f          *diskMaintFakes
	now        time.Time
	quietCalls int
}

// newDiskMaintAlertOnly builds that patrol: busy, outside the window at 06:45,
// with the trim's interval spent.
func newDiskMaintAlertOnly(t *testing.T) *diskMaintAlertOnly {
	t.Helper()
	d, logs, f := diskMaintTestDaemon(t)
	h := &diskMaintAlertOnly{d: d, logs: logs, f: f, now: diskMaintNow.Add(2 * time.Hour)}
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass gt-busy)"
	d.maint.quiet = func(*Daemon) (bool, string) {
		h.quietCalls++
		return f.quiet, f.quietWhy
	}
	d.diskMaint.now = func() time.Time { return h.now }
	d.diskMaintSchedule.record(diskMaintCacheTrimName, h.now)
	return h
}

// reads counts the disk reads the patrol makes and serves it the harness's
// readings.
func (h *diskMaintAlertOnly) reads() *atomic.Int32 {
	n := new(atomic.Int32)
	h.d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		n.Add(1)
		return h.f.readings
	}
	return n
}

// logLineCounter counts the lines a logger wrote, safe to read while a
// dispatched pass still holds a goroutine of its own.
type logLineCounter struct {
	mu    sync.Mutex
	lines int
}

func (w *logLineCounter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines += bytes.Count(p, []byte("\n"))
	return len(p), nil
}

func (w *logLineCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lines
}

// TestDiskMaintAlertOnlyRunsOnABusyTickWithNothingDue: a busy town with nothing
// due used to read the disks only when the trim came due, hours apart. The tick
// reads them for the alerts alone: one read, the alert raised, and no schedule
// entry written (gt-gg9hv.53).
func TestDiskMaintAlertOnlyRunsOnABusyTickWithNothingDue(t *testing.T) {
	t.Parallel()
	h := newDiskMaintAlertOnly(t)
	reads := h.reads()
	h.f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}

	h.d.runDiskMaintenance()

	if got := reads.Load(); got != 1 {
		t.Fatalf("disk reads = %d, want 1: the busy tick with nothing due reads the disks for the alerts", got)
	}
	if !diskMaintAlertIsOpen(h.f.notify, "diskmaint:colima") {
		t.Errorf("escalations = %+v, want an open diskmaint:colima alert", h.f.notify.Escalations())
	}
	if got := len(h.f.target.policies); got != 0 {
		t.Errorf("planned the target %d times, want 0: the alert-only pass plans nothing", got)
	}
	if got := h.f.targets; got != 0 {
		t.Errorf("targets built = %d, want 0", got)
	}
	// The trim's last run is the one the harness recorded, not a new one, and
	// no retry or target entry is written either.
	if got, want := h.d.diskMaintSchedule.lastRunAt(diskMaintCacheTrimName), h.now; !got.Equal(want) {
		t.Errorf("cache trim last run = %v, want %v untouched", got, want)
	}
	if got := h.d.diskMaintSchedule.retryAtOf(diskMaintCacheTrimName); !got.IsZero() {
		t.Errorf("cache trim retry at = %v, want none", got)
	}
	if got := h.d.diskMaintSchedule.lastRunAt("registry"); !got.IsZero() {
		t.Errorf("registry last run = %v, want none: the alert-only pass records no target", got)
	}
}

// TestDiskMaintAlertOnlyRunsWhenATargetIsDueOnABusyTick: a busy tick with a
// target due and the trim not due dispatched nothing that read the disks, so
// the target's wait for a quiet tick starved the alerts. The tick reads them
// for the alerts alone, and the target stays due (gt-gg9hv.53).
func TestDiskMaintAlertOnlyRunsWhenATargetIsDueOnABusyTick(t *testing.T) {
	t.Parallel()
	h := newDiskMaintAlertOnly(t)
	// The registry runs on a cadence of its own, and has never run: it is due,
	// while the trim's interval is spent.
	h.d.patrolConfig = diskMaintCadence("registry", "1h")
	reads := h.reads()
	h.f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}

	h.d.runDiskMaintenance()

	if got := reads.Load(); got != 1 {
		t.Fatalf("disk reads = %d, want 1: the busy tick with a target due reads the disks for the alerts", got)
	}
	if !diskMaintAlertIsOpen(h.f.notify, "diskmaint:colima") {
		t.Errorf("escalations = %+v, want an open diskmaint:colima alert", h.f.notify.Escalations())
	}
	if got := len(h.f.target.policies); got != 0 {
		t.Errorf("planned the target %d times while the town was busy, want 0: it waits for a quiet tick", got)
	}
	if !strings.Contains(h.logs.String(), "skipped") {
		t.Errorf("log = %q, want the skipped target named", h.logs.String())
	}
	if got := h.d.diskMaintSchedule.lastRunAt("registry"); !got.IsZero() {
		t.Errorf("registry last run = %v, want none: the alert-only pass records no target", got)
	}
	if got, want := h.d.diskMaintSchedule.lastRunAt(diskMaintCacheTrimName), h.now; !got.Equal(want) {
		t.Errorf("cache trim last run = %v, want %v untouched", got, want)
	}
}

// TestDiskMaintAlertOnlyIsSpacedAndSharesTheClock: the alert-only pass runs at
// most once per diskMaintBusyAlertEvery, measured from the last application any
// pass made, and a tick inside the spacing makes no quiet call. A reading that
// did not apply restarts nothing (gt-gg9hv.53).
func TestDiskMaintAlertOnlyIsSpacedAndSharesTheClock(t *testing.T) {
	t.Parallel()

	t.Run("spaced", func(t *testing.T) {
		t.Parallel()
		h := newDiskMaintAlertOnly(t)
		reads := h.reads()
		h.f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}

		h.d.runDiskMaintenance()
		if got := reads.Load(); got != 1 {
			t.Fatalf("disk reads = %d after the first tick, want 1", got)
		}
		quietCalls := h.quietCalls

		h.now = h.now.Add(diskMaintBusyAlertEvery - time.Minute)
		h.d.runDiskMaintenance()
		if got := reads.Load(); got != 1 {
			t.Errorf("disk reads = %d inside the spacing, want 1", got)
		}
		if got := h.quietCalls; got != quietCalls {
			t.Errorf("quiet guard calls = %d inside the spacing, want %d: a tick inside it makes no quiet call", got, quietCalls)
		}

		h.now = h.now.Add(time.Minute) // exactly diskMaintBusyAlertEvery after the application
		h.d.runDiskMaintenance()
		if got := reads.Load(); got != 2 {
			t.Errorf("disk reads = %d past the spacing, want 2", got)
		}
	})

	t.Run("a trim pass restarts it", func(t *testing.T) {
		t.Parallel()
		h := newDiskMaintAlertOnly(t)
		reads := h.reads()
		countCacheTrims(h.d)
		h.f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}

		h.now = h.now.Add(2 * time.Hour) // the trim is due again
		h.d.runDiskMaintenance()
		if got := reads.Load(); got != 1 {
			t.Fatalf("disk reads = %d on the trim pass, want 1", got)
		}

		h.now = h.now.Add(time.Minute) // inside the spacing the trim's application restarted
		h.d.runDiskMaintenance()
		if got := reads.Load(); got != 1 {
			t.Errorf("disk reads = %d a minute after the trim applied, want 1: its application restarted the spacing", got)
		}
	})

	t.Run("a fallback read spends its own spacing", func(t *testing.T) {
		t.Parallel()
		h := newDiskMaintAlertOnly(t)
		reads := h.reads()
		h.f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}
		h.d.diskMaintAlerts.acquireTurn = func(context.Context) (func(), error) {
			return nil, context.DeadlineExceeded
		}

		h.d.runDiskMaintenance() // reads for itself, applies nothing
		if got := reads.Load(); got != 1 {
			t.Fatalf("disk reads = %d on the fallback, want 1", got)
		}
		if diskMaintAlertIsOpen(h.f.notify, "diskmaint:colima") {
			t.Fatal("an alert is open after a fallback read, want none: the pass never took the turn")
		}

		h.now = h.now.Add(5 * time.Minute)
		h.d.runDiskMaintenance()
		if got := reads.Load(); got != 1 {
			t.Errorf("disk reads = %d inside the spacing, want 1: the pass's own read spends it even without the turn", got)
		}

		h.now = h.now.Add(5 * time.Minute) // diskMaintBusyAlertEvery after the fallback read
		h.d.runDiskMaintenance()
		if got := reads.Load(); got != 2 {
			t.Errorf("disk reads = %d past the spacing, want 2: a fallback read spaces the next one, it does not end the pass", got)
		}
	})
}

// TestDiskMaintAlertOnlySingleFlight: the alert-only pass has a guard of its
// own. A pass still in flight keeps the next tick from starting a second, and
// that tick says nothing; once the pass ends the guard is free for the tick
// past the spacing (gt-gg9hv.53).
func TestDiskMaintAlertOnlySingleFlight(t *testing.T) {
	t.Parallel()
	h := newDiskMaintAlertOnly(t)
	blocked := &blockedNotifier{Notifier: h.f.notify, entered: make(chan struct{}), release: make(chan struct{})}
	h.d.notifier = blocked
	h.f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}
	lines := &logLineCounter{}
	h.d.logger = log.New(lines, "", 0)
	release := sync.OnceFunc(func() { close(blocked.release) })
	t.Cleanup(release)
	var pending []func()
	h.d.diskMaint.dispatch = func(fn func()) { pending = append(pending, fn) }

	h.d.runDiskMaintenance()
	if len(pending) != 1 {
		t.Fatalf("dispatched %d passes, want 1: the busy tick's alert-only pass", len(pending))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pending[0]()
	}()
	awaitOrFail(t, blocked.entered, "the alert-only pass to reach the notifier")

	said := lines.count()
	h.d.runDiskMaintenance() // the pass is still in the notifier
	if len(pending) != 1 {
		t.Errorf("dispatched %d passes, want 1: a second alert-only pass started while one was in flight", len(pending))
	}
	if got := lines.count(); got != said {
		t.Errorf("logged %d lines, want %d: a tick whose alert-only pass is in flight says nothing", got, said)
	}

	release()
	awaitOrFail(t, done, "the alert-only pass to finish once the notifier answered")
	h.now = h.now.Add(diskMaintBusyAlertEvery)
	h.d.runDiskMaintenance()
	if len(pending) != 2 {
		t.Fatalf("dispatched %d passes, want 2: the guard is free once the first pass ended", len(pending))
	}
}

// TestDiskMaintAlertOnlyIsBoundedByTheTurnGate: the alert-only pass waits for
// the alert turn for at most diskMaintAlertTurnWait. When the turn never comes
// it reads the disks, calls no notifier, says so once an hour, and leaves the
// trim's guard and schedule as they were (gt-gg9hv.53).
func TestDiskMaintAlertOnlyIsBoundedByTheTurnGate(t *testing.T) {
	t.Parallel()
	h := newDiskMaintAlertOnly(t)
	reads := h.reads()
	h.f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}
	counting := &countingNotifier{Notifier: h.f.notify}
	h.d.notifier = counting
	// A turn that never answers: the fake reads the wait's deadline and gives
	// up at it, so the bound is driven without the test spending it.
	var waitDeadline time.Time
	hasDeadline := false
	h.d.diskMaintAlerts.acquireTurn = func(ctx context.Context) (func(), error) {
		waitDeadline, hasDeadline = ctx.Deadline()
		return nil, context.DeadlineExceeded
	}

	h.d.runDiskMaintenance()
	if !hasDeadline {
		t.Fatal("the turn wait has no deadline, want one of diskMaintAlertTurnWait")
	}
	if wait := time.Until(waitDeadline); wait > diskMaintAlertTurnWait || wait < diskMaintAlertTurnWait-time.Minute {
		t.Errorf("turn wait = %v, want about %v: the pass is bounded below its own %v", wait, diskMaintAlertTurnWait, diskMaintRunTimeout)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("disk reads = %d, want 1: a pass without the turn reads the disks for itself", got)
	}
	if got := counting.escalations.Load(); got != 0 {
		t.Errorf("the notifier was called %d times, want 0: the alerts stay with the pass that holds the turn", got)
	}
	if got := strings.Count(h.logs.String(), "another pass is applying the disk alerts"); got != 1 {
		t.Errorf("logged the fallback %d times, want 1: %q", got, h.logs.String())
	}
	if h.d.diskMaintTrimRunning.Load() {
		t.Error("the trim's guard is held, want it untouched by the alert-only pass")
	}
	if got, want := h.d.diskMaintSchedule.lastRunAt(diskMaintCacheTrimName), h.now; !got.Equal(want) {
		t.Errorf("cache trim last run = %v, want %v untouched", got, want)
	}

	// Five minutes on the tick makes no pass: the fallback read spent the
	// spacing. Ten minutes after it the pass reads again, and the line stays
	// said: a busy town must not repeat it every ten minutes.
	h.now = h.now.Add(5 * time.Minute)
	h.d.runDiskMaintenance()
	if got := reads.Load(); got != 1 {
		t.Errorf("disk reads = %d inside the spacing a fallback read spent, want 1", got)
	}
	h.now = h.now.Add(5 * time.Minute) // diskMaintBusyAlertEvery after the fallback read
	h.d.runDiskMaintenance()
	if got := reads.Load(); got != 2 {
		t.Errorf("disk reads = %d past the spacing, want 2", got)
	}
	if got := strings.Count(h.logs.String(), "another pass is applying the disk alerts"); got != 1 {
		t.Errorf("logged the fallback %d times inside the hour, want 1: %q", got, h.logs.String())
	}

	// An hour after the first, it is said again.
	h.now = h.now.Add(time.Hour)
	h.d.runDiskMaintenance()
	if got := strings.Count(h.logs.String(), "another pass is applying the disk alerts"); got != 2 {
		t.Errorf("logged the fallback %d times an hour on, want 2: %q", got, h.logs.String())
	}
}

// TestDiskMaintAlertOnlyDoesNotRunWhenAPassWillRead: the alert-only pass is for
// a tick nothing else reads on. A busy tick with the trim due runs the trim
// pass alone, a quiet tick with nothing due runs nothing, and an inactive
// patrol reads nothing (gt-gg9hv.53).
func TestDiskMaintAlertOnlyDoesNotRunWhenAPassWillRead(t *testing.T) {
	t.Parallel()

	t.Run("a busy tick with the trim due runs the trim pass alone", func(t *testing.T) {
		t.Parallel()
		d, _, f := diskMaintTestDaemon(t)
		trims := countCacheTrims(d)
		var reads atomic.Int32
		d.diskMaint.readDisks = func(context.Context) []disks.Reading {
			reads.Add(1)
			return []disks.Reading{{Name: "colima", UsedPct: 88}}
		}
		f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass gt-busy)"
		var pending []func()
		d.diskMaint.dispatch = func(fn func()) { pending = append(pending, fn) }

		d.runDiskMaintenance() // 04:45: the trim has never run, so it is due
		if len(pending) != 1 {
			t.Fatalf("dispatched %d passes, want 1: the tick's trim pass", len(pending))
		}
		pending[0]()

		if got := *trims; got != 1 {
			t.Errorf("trimmed %d times, want 1: a tick with the trim due runs the trim pass, not the alert-only pass", got)
		}
		if got := reads.Load(); got != 1 {
			t.Errorf("disk reads = %d, want 1: the trim pass reads the disks, and the alert-only pass adds no second read", got)
		}
	})

	t.Run("a quiet tick with nothing due reads nothing", func(t *testing.T) {
		t.Parallel()
		h := newDiskMaintAlertOnly(t)
		reads := h.reads()
		h.f.quiet = true
		var pending []func()
		h.d.diskMaint.dispatch = func(fn func()) { pending = append(pending, fn) }

		h.d.runDiskMaintenance()

		if len(pending) != 0 {
			t.Errorf("dispatched %d passes, want 0: a quiet tick with nothing due has nothing to do", len(pending))
		}
		if got := reads.Load(); got != 0 {
			t.Errorf("disk reads = %d, want 0", got)
		}
	})

	t.Run("an inactive patrol reads nothing", func(t *testing.T) {
		t.Parallel()
		h := newDiskMaintAlertOnly(t)
		reads := h.reads()
		h.d.patrolConfig = diskMaintEnabled("", "")
		h.d.patrolConfig.Patrols.DiskMaintenance.Enabled = false

		h.d.runDiskMaintenance()

		if got := reads.Load(); got != 0 {
			t.Errorf("disk reads = %d, want 0: an inactive patrol reads nothing", got)
		}
	})
}

// TestDiskMaintTrimPassIsSingleFlight: the trim has a guard of its own. A trim
// pass in flight keeps the next tick from starting a second, and a target pass
// in flight does not keep a due trim off (gt-gg9hv.48).
func TestDiskMaintTrimPassIsSingleFlight(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass dv-x)"
	now := diskMaintNow
	d.diskMaint.now = func() time.Time { return now }
	// Capture dispatched passes instead of running them, so a pass is still in
	// flight when the next tick arrives.
	var pending []func()
	d.diskMaint.dispatch = func(fn func()) { pending = append(pending, fn) }

	d.runDiskMaintenance() // 04:45, busy: the trim pass is dispatched and held
	if len(pending) != 1 {
		t.Fatalf("dispatched %d passes, want 1: the busy tick's trim pass", len(pending))
	}
	d.runDiskMaintenance() // the trim pass has not concluded
	if len(pending) != 1 {
		t.Fatalf("dispatched %d passes, want 1: a second trim pass started while one was in flight", len(pending))
	}
	if *trims != 0 {
		t.Errorf("trimmed %d times before the dispatched pass ran, want 0", *trims)
	}
	pending[0]() // the trim pass concludes and releases its guard
	if *trims != 1 {
		t.Fatalf("trimmed %d times once the trim pass ran, want 1", *trims)
	}

	// A quiet tick now dispatches the combined pass for the day's target, and
	// that pass stays in flight: its guard is the target pass's own.
	f.quiet, f.quietWhy = true, ""
	d.runDiskMaintenance()
	if len(pending) != 2 {
		t.Fatalf("dispatched %d passes, want 2: the quiet tick's combined pass", len(pending))
	}

	// Two hours on the trim is due again while the target pass still runs: the
	// trim runs anyway, which it could not if it shared that pass's guard.
	now = now.Add(2 * time.Hour)
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass dv-x)"
	d.runDiskMaintenance()
	if len(pending) != 3 {
		t.Fatalf("dispatched %d passes, want 3: a target pass in flight must not hold the trim off", len(pending))
	}
	pending[2]()
	if *trims != 2 {
		t.Fatalf("trimmed %d times, want 2", *trims)
	}

	// The target pass's own snapshot predates the trim, so finishing it trims
	// nothing a second time.
	pending[1]()
	if *trims != 2 {
		t.Errorf("trimmed %d times after the combined pass finished, want 2", *trims)
	}
}

// alertTurns drives a busy tick's trim pass and a quiet tick's combined pass
// into their alert applications at once and returns the notifier the alerts
// reached and the most disk reads that were in flight together. The trim pass's
// read is held by the test, so the combined pass arrives at its turn while the
// first application is still running. With noGate the turn is granted to every
// pass at once, so the two reads overlap; with the real gate the second pass
// waits for the first.
func alertTurns(t *testing.T, noGate bool) (rec *notifyfake.Recorder, most int32) {
	t.Helper()
	d, _, f := diskMaintTestDaemon(t)
	// The trim itself reaches no claim: the passes under test are their alert
	// applications and what follows them.
	countCacheTrims(d)
	reached := make(chan struct{})
	var turns atomic.Int32
	d.diskMaintAlerts.acquireTurn = func(ctx context.Context) (func(), error) {
		if turns.Add(1) == 2 {
			close(reached) // the second pass has arrived at its turn
		}
		if noGate {
			return func() {}, nil
		}
		return d.diskMaintAlerts.takeGate(ctx)
	}

	var quiet atomic.Bool
	d.maint.quiet = func(*Daemon) (bool, string) {
		if quiet.Load() {
			return true, ""
		}
		return false, "daemon has work in flight (landing pass dv-x)"
	}
	var passes []func()
	d.diskMaint.dispatch = func(fn func()) { passes = append(passes, fn) }

	first := make(chan struct{})
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	var reads, inFlight, peak atomic.Int32
	d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		n := reads.Add(1)
		cur := inFlight.Add(1)
		for {
			if m := peak.Load(); cur <= m || peak.CompareAndSwap(m, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		if n == 1 {
			close(first)
			waitCtx, cancelWait := context.WithTimeout(context.Background(), awaitOrFailTimeout)
			defer cancelWait()
			select {
			case <-hold:
			case <-waitCtx.Done():
				t.Errorf("waited %s for the test to release the held disk read; it never came", awaitOrFailTimeout)
			}
			return []disks.Reading{{Name: "mac", UsedPct: 85}}
		}
		return []disks.Reading{{Name: "colima", UsedPct: 88}}
	}

	d.runDiskMaintenance() // busy: the trim pass alone
	if len(passes) != 1 {
		t.Fatalf("dispatched %d passes, want 1: the busy tick's trim pass", len(passes))
	}
	trimDone := make(chan struct{})
	go func() {
		defer close(trimDone)
		passes[0]()
	}()
	awaitOrFail(t, first, "the trim pass to reach its held disk read")

	quiet.Store(true)
	d.runDiskMaintenance() // quiet: the combined pass
	if len(passes) != 2 {
		t.Fatalf("dispatched %d passes, want 2: the quiet tick's combined pass", len(passes))
	}
	targetDone := make(chan struct{})
	go func() {
		defer close(targetDone)
		passes[1]()
	}()
	awaitOrFail(t, reached, "the combined pass to reach its turn")
	if noGate {
		// Nothing holds the combined pass back, so it finishes beside the held
		// read: waiting for it is what makes the overlap certain.
		awaitOrFail(t, targetDone, "the combined pass to run beside the held read")
	} else if got := reads.Load(); got != 1 {
		t.Errorf("%d disk reads started with the combined pass waiting for its turn, want 1: it read before the first application finished", got)
	}
	release()
	awaitOrFail(t, trimDone, "the trim pass to finish")
	awaitOrFail(t, targetDone, "the combined pass to finish")
	return f.notify, peak.Load()
}

// TestDiskMaintAlertApplicationsRunOneAfterAnother: the trim pass and the
// combined pass can run at the same time, and their alert applications take
// turns. The later pass reads after the earlier one has applied its own, so no
// reading is older than the state it is applied over, no raise is lost, and a
// raise and a clear for one key reach the notifier in the order they were
// decided (gt-gg9hv.48).
func TestDiskMaintAlertApplicationsRunOneAfterAnother(t *testing.T) {
	t.Parallel()
	rec, peak := alertTurns(t, false)
	if peak != 1 {
		t.Errorf("%d disk reads in flight at once, want 1: the applications overlapped", peak)
	}
	for _, key := range []string{"diskmaint:mac", "diskmaint:colima"} {
		if !diskMaintAlertIsOpen(rec, key) {
			t.Errorf("alert %s is not open: a raise was lost", key)
		}
	}
}

// TestDiskMaintAlertTurnTestDetectsAMissingGate: the test above is only worth
// its name if it goes red when the gate goes. The same body with every pass
// granted its turn at once must see the two reads overlap (gt-gg9hv.48).
func TestDiskMaintAlertTurnTestDetectsAMissingGate(t *testing.T) {
	t.Parallel()
	if _, peak := alertTurns(t, true); peak <= 1 {
		t.Fatalf("%d disk reads in flight with the gate removed; the ordering test cannot detect a missing gate", peak)
	}
}

// blockedNotifier holds its first escalation until the test releases it, so a
// test can keep an alert application in the notifier while the rest of the pass
// goes on.
type blockedNotifier struct {
	notify.Notifier
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (n *blockedNotifier) Escalate(ctx context.Context, e notify.Escalation) error {
	n.once.Do(func() { close(n.entered) })
	bound, cancelBound := context.WithTimeout(context.Background(), awaitOrFailTimeout)
	defer cancelBound()
	select {
	case <-n.release:
	case <-bound.Done():
		return errors.New("the blocked notifier was never released by the test")
	}
	return n.Notifier.Escalate(ctx, e)
}

// TestDiskMaintBlockedNotifierDoesNotHoldTheTrim: the trim pass applies the
// alerts beside the trim, so a notifier that does not answer delays the end of
// the alerts and not the trim, which runs while the notifier is still blocked
// (gt-gg9hv.48).
func TestDiskMaintBlockedNotifierDoesNotHoldTheTrim(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	blocked := &blockedNotifier{Notifier: f.notify, entered: make(chan struct{}), release: make(chan struct{})}
	d.notifier = blocked
	f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass dv-x)"
	trimmed := make(chan struct{})
	d.diskMaint.trim = func(context.Context, string) bool {
		close(trimmed)
		return true
	}
	var passes []func()
	d.diskMaint.dispatch = func(fn func()) { passes = append(passes, fn) }
	release := sync.OnceFunc(func() { close(blocked.release) })
	t.Cleanup(release)

	d.runDiskMaintenance()
	if len(passes) != 1 {
		t.Fatalf("dispatched %d passes, want 1: the busy tick's trim pass", len(passes))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		passes[0]()
	}()
	awaitOrFail(t, blocked.entered, "the alert application to reach the notifier")
	awaitOrFail(t, trimmed, "the trim to run while the notifier is blocked")
	release()
	awaitOrFail(t, done, "the trim pass to finish once the notifier answered")
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("no alert open for a disk at 88% once the notifier answered")
	}
}

// TestDiskMaintBlockedNotifierKeepsNoLaterTrimOff: the trim's guard is handed
// back when the trim ends, not when the alerts do. With the notifier still
// blocked on the first pass's alert, the trim that comes due again runs on a
// pass of its own (gt-gg9hv.48).
func TestDiskMaintBlockedNotifierKeepsNoLaterTrimOff(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	blocked := &blockedNotifier{Notifier: f.notify, entered: make(chan struct{}), release: make(chan struct{})}
	d.notifier = blocked
	f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass dv-x)"
	var trims atomic.Int32
	firstTrimmed := make(chan struct{})
	d.diskMaint.trim = func(context.Context, string) bool {
		if trims.Add(1) == 1 {
			close(firstTrimmed)
		}
		return true
	}
	// The first pass is still reading the clock from its alert goroutine when the
	// test moves it on, so the clock is atomic.
	var clock atomic.Int64
	clock.Store(diskMaintNow.UnixNano())
	d.diskMaint.now = func() time.Time { return time.Unix(0, clock.Load()).In(diskMaintNow.Location()) }
	var passes []func()
	d.diskMaint.dispatch = func(fn func()) { passes = append(passes, fn) }
	release := sync.OnceFunc(func() { close(blocked.release) })
	t.Cleanup(release)

	d.runDiskMaintenance() // the first trim pass: its alert blocks in the notifier
	if len(passes) != 1 {
		t.Fatalf("dispatched %d passes, want 1", len(passes))
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		passes[0]()
	}()
	awaitOrFail(t, blocked.entered, "the first pass's alert to reach the notifier")
	awaitOrFail(t, firstTrimmed, "the first trim")

	// Two hours on the trim is due again, while the first pass is still waiting
	// on its notifier: the guard was handed back, so a second trim pass starts.
	clock.Add(int64(2 * time.Hour))
	d.runDiskMaintenance()
	if len(passes) != 2 {
		t.Fatalf("dispatched %d passes, want 2: the guard must be free once the first trim ended", len(passes))
	}
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		passes[1]()
	}()
	release()
	awaitOrFail(t, firstDone, "the first pass to finish once the notifier answered")
	awaitOrFail(t, secondDone, "the second pass to finish")
	if got := trims.Load(); got != 2 {
		t.Errorf("trimmed %d times, want 2: the later trim ran while the first alert was blocked", got)
	}
}

// countingNotifier counts the escalations that reach the notifier at all,
// whether or not the notifier then accepts them.
type countingNotifier struct {
	notify.Notifier
	escalations atomic.Int32
}

func (n *countingNotifier) Escalate(ctx context.Context, e notify.Escalation) error {
	n.escalations.Add(1)
	return n.Notifier.Escalate(ctx, e)
}

// TestAlertSendsEndWithTheirPassContext: a raise or a clear sent under a context
// that has already ended is abandoned with that context's error and reaches the
// notifier not at all, so a pass that has ended leaves the alert as it was and
// the next pass decides again (gt-gg9hv.48).
func TestAlertSendsEndWithTheirPassContext(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	counting := &countingNotifier{Notifier: f.notify}
	d.notifier = counting
	ended, cancel := context.WithCancel(context.Background())
	cancel()

	err := d.escalateAlertCtx(ended, "diskmaint:colima", diskMaintAlertSource, "colima is at 88%")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("escalateAlertCtx on an ended context = %v, want context.Canceled", err)
	}
	if diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("an alert is open after a raise sent on an ended context: it reached the notifier")
	}
	if got := counting.escalations.Load(); got != 0 {
		t.Errorf("the notifier was called %d times for a raise on an ended context, want 0: no attempt is spent on a send nobody waits for", got)
	}

	// The same alert raised on a live context, then cleared on an ended one,
	// stays open; cleared on a live one, it closes.
	if err := d.escalateAlertCtx(context.Background(), "diskmaint:colima", diskMaintAlertSource, "colima is at 88%"); err != nil {
		t.Fatalf("escalateAlertCtx on a live context = %v", err)
	}
	if err := d.clearAlertsCtx(ended, "colima is back under 80%", "diskmaint:colima"); !errors.Is(err, context.Canceled) {
		t.Errorf("clearAlertsCtx on an ended context = %v, want context.Canceled", err)
	}
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("the alert closed after a clear sent on an ended context: it reached the notifier")
	}
	if err := d.clearAlertsCtx(context.Background(), "colima is back under 80%", "diskmaint:colima"); err != nil {
		t.Fatalf("clearAlertsCtx on a live context = %v", err)
	}
	if diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("the alert is still open after a clear on a live context")
	}
}

// TestDiskMaintCombinedPassGoesOnWhenTheTurnIsNotGranted: a pass that waits for
// its turn to apply the alerts and does not get it in time reads the disks for
// itself and carries on to its targets, leaving the alerts to the pass that has
// the turn (gt-gg9hv.48). The readings of that fallback read are the ones the
// pass decides with, so the keep it plans is the tightened one (gt-gg9hv.54).
func TestDiskMaintCombinedPassGoesOnWhenTheTurnIsNotGranted(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	countCacheTrims(d)
	var reads atomic.Int32
	d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		reads.Add(1)
		return []disks.Reading{{Name: "colima", UsedPct: 88}}
	}
	// The turn is not granted within the wait: the wait's own deadline has
	// passed, reported at once so the test does not spend the real wait.
	d.diskMaintAlerts.acquireTurn = func(context.Context) (func(), error) {
		return nil, context.DeadlineExceeded
	}
	d.runDiskMaintenance()
	if len(f.target.policies) != 1 {
		t.Fatalf("planned the target %d times, want 1: a pass without the alert turn still plans its targets", len(f.target.policies))
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("disk reads = %d, want 1: the fallback reads the disks once, for its own decisions", got)
	}
	// 3 is the tightened keep a disk at or over 80% asks for; the pass starts
	// from the default 5.
	if keep := f.target.policies[0].Keep; keep != 3 {
		t.Errorf("planned keep = %d, want 3: the keep decision did not use the fallback's readings of a disk at 88%%", keep)
	}
	if diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("an alert is open: the pass without the turn applied the alerts")
	}
	if !strings.Contains(logs.String(), "another pass is applying the disk alerts") {
		t.Errorf("log = %q, want the read-only fallback named", logs.String())
	}
}

// TestDiskMaintCombinedPassEndsWhenItsContextEndsWhileWaitingForTheTurn: a pass
// whose own context ends while it waits for the alert turn is over, not
// fallback: it reads no disk, applies nothing, plans nothing, and leaves its
// targets due for the next tick (gt-gg9hv.54).
func TestDiskMaintCombinedPassEndsWhenItsContextEndsWhileWaitingForTheTurn(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	countCacheTrims(d)
	var reads atomic.Int32
	d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		reads.Add(1)
		return []disks.Reading{{Name: "colima", UsedPct: 88}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	entered := make(chan struct{})
	d.diskMaintAlerts.acquireTurn = func(waitCtx context.Context) (func(), error) {
		close(entered)
		<-waitCtx.Done()
		return nil, waitCtx.Err()
	}

	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		d.runDiskMaintenance()
	}()
	awaitOrFail(t, entered, "the pass to reach its turn wait")
	cancel() // the pass's own context, not the wait's deadline
	awaitOrFail(t, tickDone, "the pass to end with its context")

	if strings.Contains(logs.String(), "another pass is applying the disk alerts") {
		t.Errorf("log = %q, want no read-only fallback: the pass itself was over", logs.String())
	}
	if !strings.Contains(logs.String(), "the disk alerts were not applied") {
		t.Errorf("log = %q, want the unapplied alerts named", logs.String())
	}
	if got := reads.Load(); got != 0 {
		t.Errorf("disk reads = %d, want 0: a pass that ended while waiting for the turn reads nothing", got)
	}
	if got := len(f.target.policies); got != 0 {
		t.Errorf("planned the target %d times, want 0: a pass that ended plans nothing", got)
	}

	// The next tick, with a live context, finds the target still due.
	d.ctx = context.Background()
	d.diskMaintAlerts.acquireTurn = nil
	d.runDiskMaintenance()
	if got := len(f.target.policies); got != 1 {
		t.Errorf("planned the target %d times on the next tick, want 1: an ended pass leaves its targets due", got)
	}
}

// blockingNotifier blocks in its first send until the context it was handed
// ends, so a test can end a pass under a send. Later sends go to the wrapped
// notifier, which is how the next pass is seen to raise the alert again.
type blockingNotifier struct {
	notify.Notifier
	ctx    chan context.Context
	err    chan error
	blocks atomic.Bool
}

func newBlockingNotifier(wrapped notify.Notifier) *blockingNotifier {
	n := &blockingNotifier{
		Notifier: wrapped,
		ctx:      make(chan context.Context, 1),
		err:      make(chan error, 1),
	}
	n.blocks.Store(true)
	return n
}

func (n *blockingNotifier) Escalate(ctx context.Context, e notify.Escalation) error {
	if !n.blocks.CompareAndSwap(true, false) {
		return n.Notifier.Escalate(ctx, e)
	}
	n.ctx <- ctx
	<-ctx.Done()
	n.err <- ctx.Err()
	return ctx.Err()
}

// passCtxMark is carried by the pass context so the test can see that the send
// it blocks runs under a context derived from that pass.
type passCtxMark struct{}

// diskMaintNoSleepClock tells time from a fake clock and returns at once from a
// sleep, so the escalation retry's backoff is a wait the test does not spend.
type diskMaintNoSleepClock struct {
	*clockwork.FakeClock
}

func (*diskMaintNoSleepClock) Sleep(time.Duration) {}

// TestDiskMaintPassCancelAbandonsABlockedAlertSend: ending a pass's context
// while a notifier is inside the send ends the send, leaves the alert
// unrecorded for the next pass to raise again, and lets the pass finish. The
// send runs under the pass's context, not context.Background() (gt-gg9hv.54).
func TestDiskMaintPassCancelAbandonsABlockedAlertSend(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	countCacheTrims(d)
	f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}
	blocking := newBlockingNotifier(f.notify)
	d.notifier = blocking
	d.clock = &diskMaintNoSleepClock{FakeClock: clockwork.NewFakeClockAt(diskMaintNow)} // the retry's backoff
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), passCtxMark{}, "the pass"))
	d.ctx = ctx

	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		d.runDiskMaintenance()
	}()
	got := awaitOrFail(t, blocking.ctx, "the notifier's send to begin")
	if v := got.Value(passCtxMark{}); v != "the pass" {
		t.Errorf("the notifier's context carries %v, want the pass's own context: applyDiskAlerts was handed another context", v)
	}
	cancel() // end the pass under the send
	if err := awaitOrFail(t, blocking.err, "the send to end with the pass"); !errors.Is(err, context.Canceled) {
		t.Errorf("the send ended with %v, want context.Canceled: it was not derived from the pass context", err)
	}
	awaitOrFail(t, tickDone, "the pass to finish")

	if diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("an alert is open: a send abandoned with its pass recorded the alert")
	}

	// The next day's pass raises the alert again: the abandoned send left the
	// key undecided rather than open.
	d.ctx = context.Background()
	d.diskMaint.now = func() time.Time { return diskMaintNow.Add(24 * time.Hour) }
	d.runDiskMaintenance()
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("no alert open on the next pass: an abandoned send must leave its alert to be raised again")
	}
}

// TestDiskMaintTrimDoesNotRunTwiceInOneInterval: a combined pass carries the due
// set its tick found. When a busy tick's trim pass concludes before that pass
// runs, the interval is spent and the combined pass must not trim again
// (gt-gg9hv.48).
func TestDiskMaintTrimDoesNotRunTwiceInOneInterval(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	now := diskMaintNow
	d.diskMaint.now = func() time.Time { return now }
	var pending []func()
	d.diskMaint.dispatch = func(fn func()) { pending = append(pending, fn) }

	// The quiet tick dispatches the combined pass and holds it.
	f.quiet, f.quietWhy = true, ""
	d.runDiskMaintenance()
	if len(pending) != 1 {
		t.Fatalf("dispatched %d passes, want 1: the quiet tick's combined pass", len(pending))
	}
	// A busy tick follows while that pass is held: its trim pass is dispatched
	// and runs to the end first.
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass dv-x)"
	d.runDiskMaintenance()
	if len(pending) != 2 {
		t.Fatalf("dispatched %d passes, want 2: the busy tick's trim pass", len(pending))
	}
	pending[1]()
	if *trims != 1 {
		t.Fatalf("trimmed %d times after the trim pass, want 1", *trims)
	}
	// The held combined pass now runs on its old snapshot, in the same interval.
	pending[0]()
	if *trims != 1 {
		t.Errorf("trimmed %d times after the combined pass ran on its stale due set, want 1: the interval was spent", *trims)
	}
}

// TestDiskMaintBusyTrimFailureIsLoggedOnceAndTargetsWait: a trim that fails on
// a busy tick records the short retry, so the next ticks try it again, and its
// failure line is held to once by its own key rather than repeated on every
// retry. The targets the busy ticks deferred stay due, and the disk alerts
// still go up (gt-gg9hv.48).
func TestDiskMaintBusyTrimFailureIsLoggedOnceAndTargetsWait(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.quiet, f.quietWhy = false, "daemon has work in flight (landing pass dv-x)"
	f.readings = []disks.Reading{{Name: "colima", UsedPct: 88}}
	const trimFailure = "the sandbox profiles could not be read"
	var trims atomic.Int32
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		trims.Add(1)
		return nil, errors.New(trimFailure)
	}
	now := diskMaintNow
	d.diskMaint.now = func() time.Time { return now }

	// 04:45 fails and asks to be retried in fifteen minutes; 05:00 and 05:15
	// are the retries, still in the window and all still busy.
	for _, at := range []time.Time{diskMaintNow, diskMaintNow.Add(15 * time.Minute), diskMaintNow.Add(30 * time.Minute)} {
		now = at
		d.runDiskMaintenance()
	}

	if got := trims.Load(); got != 3 {
		t.Errorf("trimmed %d times over three ticks, want 3: a failed trim records the short retry", got)
	}
	if got := strings.Count(logs.String(), "disk_maintenance: cache trim:"); got != 1 {
		t.Errorf("logged the trim failure %d times, want 1: its own key holds the line for the hour: %q", got, logs.String())
	}
	if got := strings.Count(logs.String(), "targets skipped"); got != 3 {
		t.Errorf("logged the targets' skip %d times, want 3: a busy tick still defers them", got)
	}
	if len(f.target.policies) != 0 {
		t.Errorf("planned the target %d times while the town was busy, want 0", len(f.target.policies))
	}
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:colima") {
		t.Error("no alert open for a disk at 88%: the busy tick's trim pass must raise it")
	}
}

// TestCacheTrimIntervalRefusesAnUnreadableSetting: a cache_trim_interval the
// patrol cannot schedule runs no trim, even in the window, and is said once
// rather than on every tick. It does not fall back to the 2h default.
func TestCacheTrimIntervalRefusesAnUnreadableSetting(t *testing.T) {
	t.Parallel()
	d, logs, _ := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	d.patrolConfig.Patrols.DiskMaintenance.CacheTrimInterval = "soon"

	for i := 0; i < 2; i++ {
		d.runDiskMaintenance() // 04:45, in the window
	}

	if *trims != 0 {
		t.Fatalf("trimmed %d times with an unreadable cache_trim_interval, want 0", *trims)
	}
	const setting = "patrols.disk_maintenance.cache_trim_interval"
	if got := strings.Count(logs.String(), setting); got != 1 {
		t.Errorf("logged the refused cache_trim_interval %d times, want 1: %q", got, logs.String())
	}
}

// TestDiskMaintenanceRefusesAnUnreadableCadence: an interval the patrol cannot
// schedule runs that target on no cadence at all — never on the daily window —
// and says so once rather than on every tick.
func TestDiskMaintenanceRefusesAnUnreadableCadence(t *testing.T) {
	t.Parallel()
	for name, every := range map[string]string{
		"not a duration": "soon",
		"zero":           "0s",
		"negative":       "-2h",
		"under a minute": "30s",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, logs, f := diskMaintTestDaemon(t)
			d.patrolConfig = diskMaintCadence("registry", every)

			for i := 0; i < 2; i++ {
				d.runDiskMaintenance()
			}

			if len(f.target.policies) != 0 {
				t.Fatalf("registry planned %d times with its %q interval, want 0: a bad interval does not fall back to the daily window", len(f.target.policies), every)
			}
			// The setting's path names the refusal. The value is not counted: the
			// refusal quotes it again in its own reason, so a count of the literal
			// is a count of two things.
			const setting = "patrols.disk_maintenance.targets.registry.interval"
			if got := strings.Count(logs.String(), setting); got != 1 {
				t.Errorf("logged the refused %q interval %d times, want 1: %q", every, got, logs.String())
			}
		})
	}
}

// TestDiskMaintenanceRefusesAnUnknownTarget: a target name the patrol does not
// know is refused from a config built in code the way decoding refuses it from
// the config file, so it runs nothing and is said once rather than per tick.
func TestDiskMaintenanceRefusesAnUnknownTarget(t *testing.T) {
	t.Parallel()
	for name, every := range map[string]string{
		"with a cadence": "2h",
		"without one":    "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, logs, f := diskMaintTestDaemon(t)
			d.patrolConfig = diskMaintCadence("go-build", every)
			d.diskMaint.now = func() time.Time { return diskMaintNow.Add(8 * time.Hour) }

			for i := 0; i < 2; i++ {
				d.runDiskMaintenance()
			}

			if len(f.target.policies) != 0 {
				t.Errorf("registry planned %d times, want 0: an unknown target refuses rather than opening the window", len(f.target.policies))
			}
			if got := strings.Count(logs.String(), "unknown target"); got != 1 {
				t.Errorf("refused the unknown target %d times, want 1: %q", got, logs.String())
			}
			if strings.Contains(logs.String(), "has no such target") {
				t.Errorf("log = %q, want the unknown name refused, not reported as a target this build does not produce", logs.String())
			}
		})
	}
}

// TestDiskMaintenanceSkipsWhenNotQuiet: a town with work in flight gets a log
// line and no plan, and the window stays open for a later tick.
func TestDiskMaintenanceSkipsWhenNotQuiet(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.quiet, f.quietWhy = false, "slot held by gastown/polecats/corpus"

	d.runDiskMaintenance()

	if len(f.target.policies) != 0 {
		t.Errorf("planned %d times while the town was busy, want 0", len(f.target.policies))
	}
	if !strings.Contains(logs.String(), "slot held by gastown/polecats/corpus") {
		t.Errorf("log = %q, want the reason the pass was skipped", logs.String())
	}

	f.quiet = true
	d.runDiskMaintenance()
	if len(f.target.policies) != 1 {
		t.Errorf("planned %d times after the town went quiet, want 1: a skipped tick must not spend the window", len(f.target.policies))
	}
}

// TestDiskMaintenanceDryRunNeverApplies: the default mode plans with Apply
// false and never calls Apply, even with actions to perform.
func TestDiskMaintenanceDryRunNeverApplies(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.target.plan = diskmaint.Plan{
		Actions: []diskmaint.Action{{Kind: "delete-manifest", Ref: "gastown-polecat@sha256:ab", Bytes: 10, Why: "old"}},
	}

	d.runDiskMaintenance()

	if len(f.target.policies) != 1 {
		t.Fatalf("planned %d times, want 1", len(f.target.policies))
	}
	if f.target.policies[0].Apply {
		t.Error("the plan was built with Apply true in the default mode")
	}
	if len(f.target.applied) != 0 {
		t.Errorf("applied %d plans in dry-run mode, want 0", len(f.target.applied))
	}
	if !strings.Contains(logs.String(), "dry_run") {
		t.Errorf("log = %q, want a dry_run summary line", logs.String())
	}
}

// TestDiskMaintenanceApplyModeApplies: apply mode applies the plan it just
// built, and the lock that makes that safe is the target's own.
func TestDiskMaintenanceApplyModeApplies(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("04:30", agentconfig.DiskMaintenanceModeApply)
	f.target.plan = diskmaint.Plan{
		Actions: []diskmaint.Action{{Kind: "delete-manifest", Ref: "gastown-polecat@sha256:ab", Bytes: 10, Why: "old"}},
	}
	f.target.result = diskmaint.Result{
		Target:     "registry",
		Done:       []diskmaint.Action{{Kind: "delete-manifest", Ref: "gastown-polecat@sha256:ab"}},
		BytesFreed: 10,
	}

	d.runDiskMaintenance()

	if len(f.target.policies) != 1 || !f.target.policies[0].Apply {
		t.Fatalf("policies = %+v, want one with Apply true", f.target.policies)
	}
	if len(f.target.applied) != 1 || len(f.target.applied[0].Actions) != 1 {
		t.Fatalf("applied = %+v, want the planned action once", f.target.applied)
	}
	if !strings.Contains(logs.String(), "apply: deleted 1 of 1") {
		t.Errorf("log = %q, want a summary naming what was deleted", logs.String())
	}
}

// TestDiskMaintenanceDeferredApplyRetriesInTheWindow: an apply that found the
// plugin lock held says so, and the next tick in the window tries again rather
// than waiting a day.
func TestDiskMaintenanceDeferredApplyRetriesInTheWindow(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("04:30", agentconfig.DiskMaintenanceModeApply)
	f.target.plan = diskmaint.Plan{Actions: []diskmaint.Action{{Kind: "delete-manifest", Ref: "gastown-polecat@sha256:ab"}}}
	f.target.result = diskmaint.Result{Deferred: registry.ErrDeferred.Error() + ": polecat-image lock is held"}

	d.runDiskMaintenance()
	if len(f.target.applied) != 1 {
		t.Fatalf("applied %d times, want 1", len(f.target.applied))
	}
	if !strings.Contains(logs.String(), "deferred") {
		t.Errorf("log = %q, want the deferral named", logs.String())
	}

	f.target.result = diskmaint.Result{}
	d.runDiskMaintenance()
	if len(f.target.applied) != 2 {
		t.Fatalf("applied %d times, want 2: a deferral must leave the window open", len(f.target.applied))
	}

	d.runDiskMaintenance()
	if len(f.target.applied) != 2 {
		t.Fatalf("applied %d times, want 2: a completed apply spends the window", len(f.target.applied))
	}
}

// TestDiskMaintenanceRefusedPlanIsNotApplied: a plan that could not read its
// inputs is reported in both modes, and never applied.
func TestDiskMaintenanceRefusedPlanIsNotApplied(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("04:30", agentconfig.DiskMaintenanceModeApply)
	f.target.plan = diskmaint.Plan{Refused: "read failed: records directory is missing"}

	d.runDiskMaintenance()

	if len(f.target.applied) != 0 {
		t.Errorf("applied %d refused plans, want 0", len(f.target.applied))
	}
	if !strings.Contains(logs.String(), "records directory is missing") {
		t.Errorf("log = %q, want the refusal's reason", logs.String())
	}
}

// TestDiskMaintenanceTargetsErrorSettlesTheWindow: a pass that could not build
// its targets logs once and does not read the town again every tick.
func TestDiskMaintenanceTargetsErrorSettlesTheWindow(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.targetsErr = errors.New("POLECAT_IMAGE_REGISTRY is not a host:port")

	for i := 0; i < 3; i++ {
		d.runDiskMaintenance()
	}

	if f.targets != 1 {
		t.Errorf("targets built %d times in one window, want 1", f.targets)
	}
	if got := strings.Count(logs.String(), "POLECAT_IMAGE_REGISTRY is not a host:port"); got != 1 {
		t.Errorf("logged the failure %d times, want 1: %q", got, logs.String())
	}
}

// TestDiskMaintenanceMalformedWindowSettlesTheDay: a window that does not parse
// never opens, so it must say so once instead of skipping silently every tick.
func TestDiskMaintenanceMalformedWindowSettlesTheDay(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("25:99", "")

	for i := 0; i < 3; i++ {
		d.runDiskMaintenance()
	}

	if f.targets != 0 {
		t.Errorf("targets built = %d, want 0: a malformed window runs nothing", f.targets)
	}
	if got := strings.Count(logs.String(), "25:99"); got != 1 {
		t.Errorf("logged the malformed window %d times, want 1: %q", got, logs.String())
	}
}

// TestDiskMaintenanceKeepIsTheConfiguredWindow: the plan carries the keep the
// plugin uses, the knob overrides it, and a value that is not a count refuses
// the plan rather than guessing one.
func TestDiskMaintenanceKeepIsTheConfiguredWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		env     map[string]string
		want    int
		refused bool
	}{
		{name: "default", want: 5},
		{name: "configured", env: map[string]string{"POLECAT_IMAGE_REGISTRY_KEEP": "7"}, want: 7},
		{name: "not a count", env: map[string]string{"POLECAT_IMAGE_REGISTRY_KEEP": "seven"}, refused: true},
		{name: "zero", env: map[string]string{"POLECAT_IMAGE_REGISTRY_KEEP": "0"}, refused: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, f := diskMaintTestDaemon(t)
			f.env = tt.env

			d.runDiskMaintenance()

			if tt.refused {
				if len(f.target.policies) != 0 {
					t.Fatalf("planned with a keep that is not a count: %+v", f.target.policies)
				}
				if !strings.Contains(logs.String(), "POLECAT_IMAGE_REGISTRY_KEEP") {
					t.Errorf("log = %q, want the knob named", logs.String())
				}
				return
			}
			if len(f.target.policies) != 1 {
				t.Fatalf("planned %d times, want 1", len(f.target.policies))
			}
			if got := f.target.policies[0].Keep; got != tt.want {
				t.Errorf("keep = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestDiskMaintenanceUnreadableDiskRefusesThePlan: a disk the patrol cannot
// read is not a disk it may tighten (or not tighten) for, so the pass refuses.
func TestDiskMaintenanceUnreadableDiskRefusesThePlan(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.readings = []disks.Reading{{Name: "colima", Err: errors.New("ssh: connect: refused")}}

	d.runDiskMaintenance()

	if len(f.target.policies) != 0 {
		t.Errorf("planned with an unreadable disk, want no plan: %+v", f.target.policies)
	}
	if !strings.Contains(logs.String(), "colima") {
		t.Errorf("log = %q, want the unreadable disk named", logs.String())
	}
}

// TestDiskMaintenanceReadsDisksOnEveryPass: the seam the disk reads and alerts
// arrive through is wired, not decoration.
func TestDiskMaintenanceReadsDisksOnEveryPass(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	reads := 0
	d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		reads++
		return nil
	}

	d.runDiskMaintenance()

	if reads != 1 {
		t.Errorf("disk reads = %d, want 1", reads)
	}
	if len(f.target.policies) != 1 {
		t.Errorf("planned %d times, want 1", len(f.target.policies))
	}
}

// TestDiskMaintenanceConfigDecodes: the design's block decodes as written, and
// a mode, interval, window or key the patrol cannot act on is refused at decode
// rather than read as a default.
func TestDiskMaintenanceConfigDecodes(t *testing.T) {
	t.Parallel()

	var cfg PatrolsConfig
	design := `{"disk_maintenance": {"enabled": true, "interval": "daily", "window": "04:30", "mode": "dry_run", "targets": {"registry": {"interval": "2h"}}}}`
	if err := json.Unmarshal([]byte(design), &cfg); err != nil {
		t.Fatalf("the design's block does not decode: %v", err)
	}
	dm := cfg.DiskMaintenance
	if dm == nil {
		t.Fatal("disk_maintenance decoded as absent")
	}
	if !dm.Enabled || dm.Interval != "daily" || dm.Window != "04:30" || dm.Mode != "dry_run" {
		t.Errorf("decoded %+v, want the block as written", dm)
	}
	if got := dm.Targets[agentconfig.DiskMaintenanceTargetRegistry].Interval; got != "2h" {
		t.Errorf("registry interval decoded as %q, want 2h", got)
	}

	bad := map[string]string{
		"unknown mode":       `{"disk_maintenance": {"enabled": true, "mode": "applyy"}}`,
		"unknown interval":   `{"disk_maintenance": {"interval": "hourly"}}`,
		"malformed window":   `{"disk_maintenance": {"window": "4:30pm"}}`,
		"unknown key":        `{"disk_maintenance": {"enable": true}}`,
		"unknown target":     `{"disk_maintenance": {"targets": {"go-build": {"interval": "2h"}}}}`,
		"malformed cadence":  `{"disk_maintenance": {"targets": {"registry": {"interval": "soon"}}}}`,
		"zero cadence":       `{"disk_maintenance": {"targets": {"registry": {"interval": "0s"}}}}`,
		"negative cadence":   `{"disk_maintenance": {"targets": {"registry": {"interval": "-2h"}}}}`,
		"sub-minute cadence": `{"disk_maintenance": {"targets": {"registry": {"interval": "30s"}}}}`,
		"unknown target key": `{"disk_maintenance": {"targets": {"registry": {"every": "2h"}}}}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			if err := json.Unmarshal([]byte(doc), &p); err == nil {
				t.Fatalf("%s decoded without an error: %+v", name, p.DiskMaintenance)
			}
		})
	}
}

// TestDiskMaintenanceBuiltTargetAppliesUnderThePluginLock: the target the
// patrol builds for the town defers while the polecat-image lock is held and
// deletes once it is free, so the lock is wired into the apply rather than only
// available to it.
func TestDiskMaintenanceBuiltTargetAppliesUnderThePluginLock(t *testing.T) {
	t.Parallel()
	lockDir := filepath.Join(t.TempDir(), "polecat-image.lock")
	env := map[string]string{
		// A port nothing listens on: the delete half of each case fails at the
		// connection instead of reaching a registry.
		"POLECAT_IMAGE_REGISTRY": "127.0.0.1:1",
		"POLECAT_IMAGE_LOCK":     lockDir,
	}
	getenv := func(key string) string { return env[key] }

	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	targets, err := d.diskMaintenanceTargets(agentconfig.DiskMaintenanceModeApply, getenv, func() (string, error) { return t.TempDir(), nil })
	if err != nil {
		t.Fatalf("building the targets: %v", err)
	}
	target := targets["registry"]
	if target == nil {
		t.Fatal("a town with POLECAT_IMAGE_REGISTRY set has no registry target")
	}
	plan := diskmaint.Plan{Target: "registry", Actions: []diskmaint.Action{{
		Kind: "delete-manifest",
		Ref:  "gastown-polecat@sha256:" + strings.Repeat("a", 64),
	}}}
	cfg, err := polecatimage.ConfigFromEnv(func(key string) string {
		if key == "POLECAT_IMAGE_TOWN_ROOT" {
			return d.config.TownRoot
		}
		return getenv(key)
	})
	if err != nil {
		t.Fatalf("reading the plugin config: %v", err)
	}

	held := polecatimage.WithLock(context.Background(), cfg, polecatimage.WithLockOptions{Out: io.Discard}, func(ctx context.Context) error {
		result, applyErr := target.Apply(ctx, plan)
		if result.Deferred == "" {
			t.Errorf("applying while another run holds the lock: Deferred = %q, want the deferral (err %v)", result.Deferred, applyErr)
		}
		if len(result.Done) != 0 {
			t.Errorf("deleted %d manifests while another run held the lock, want 0", len(result.Done))
		}
		return nil
	})
	if held != nil {
		t.Fatalf("holding the plugin lock: %v", held)
	}

	result, _ := target.Apply(context.Background(), plan)
	if result.Deferred != "" {
		t.Errorf("with the lock free the apply deferred again: %q", result.Deferred)
	}
	if len(result.Failed) == 0 {
		t.Errorf("with the lock free the apply did not reach the delete: result = %+v", result)
	}
}

// TestDiskMaintenanceRefusesAnUnknownMode: a mode the patrol cannot act on runs
// nothing, says why, and is not retried every tick — the decode refuses one,
// and a config built in code must not read as an apply.
func TestDiskMaintenanceRefusesAnUnknownMode(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("04:30", "applyy")

	for i := 0; i < 2; i++ {
		d.runDiskMaintenance()
	}

	if f.targets != 0 {
		t.Errorf("targets built = %d, want 0 for a mode the patrol cannot act on", f.targets)
	}
	if got := strings.Count(logs.String(), "applyy"); got != 1 {
		t.Errorf("logged the mode %d times, want 1: %q", got, logs.String())
	}
}

// TestDiskMaintenanceTargetsRefuseBadSettings: each setting the target cannot
// be built from is an error the pass reports, never a target built from a guess.
func TestDiskMaintenanceTargetsRefuseBadSettings(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return t.TempDir(), nil }
	noHome := func() (string, error) { return "", errors.New("HOME is unset") }
	cases := []struct {
		name  string
		town  string
		mode  string
		env   map[string]string
		home  func() (string, error)
		wants string
	}{
		{
			name:  "the plugin settings do not parse",
			mode:  agentconfig.DiskMaintenanceModeDryRun,
			env:   map[string]string{"POLECAT_IMAGE_STALE_MINUTES": "soon"},
			home:  home,
			wants: "POLECAT_IMAGE_STALE_MINUTES",
		},
		{
			name:  "a registry path is the retired setting",
			mode:  agentconfig.DiskMaintenanceModeDryRun,
			env:   map[string]string{"POLECAT_IMAGE_REGISTRY": "localhost:5001/x"},
			home:  home,
			wants: "not a host:port",
		},
		{
			name:  "no home for the docker socket",
			mode:  agentconfig.DiskMaintenanceModeDryRun,
			env:   map[string]string{"POLECAT_IMAGE_REGISTRY": "localhost:5001"},
			home:  noHome,
			wants: "colima docker socket",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := tt.town
			if town == "" {
				town = t.TempDir()
			}
			d := &Daemon{config: &Config{TownRoot: town}}
			_, err := d.diskMaintenanceTargets(tt.mode, func(k string) string { return tt.env[k] }, tt.home)
			if err == nil {
				t.Fatalf("%s built a target anyway", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wants) {
				t.Errorf("error = %v, want it to name %q", err, tt.wants)
			}
		})
	}
}

// TestDiskMaintenanceApplyNeedsALedgerDirectory: an apply that cannot make its
// ledger directory fails before anything is deleted, because an apply it could
// not record must not run.
func TestDiskMaintenanceApplyNeedsALedgerDirectory(t *testing.T) {
	t.Parallel()
	// A file where the runtime directory goes: .runtime/diskmaint cannot be
	// made under it.
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: town}}

	_, err := d.diskMaintenanceTargets(agentconfig.DiskMaintenanceModeApply,
		func(k string) string { return map[string]string{"POLECAT_IMAGE_REGISTRY": "localhost:5001"}[k] },
		func() (string, error) { return t.TempDir(), nil })
	if err == nil || !strings.Contains(err.Error(), "ledger directory") {
		t.Fatalf("apply with no ledger directory = %v, want the refusal", err)
	}

	// The same town may still plan: a dry run leaves the disk as it found it.
	if _, err := d.diskMaintenanceTargets(agentconfig.DiskMaintenanceModeDryRun,
		func(k string) string { return map[string]string{"POLECAT_IMAGE_REGISTRY": "localhost:5001"}[k] },
		func() (string, error) { return t.TempDir(), nil }); err != nil {
		t.Fatalf("dry run in the same town = %v, want a target", err)
	}
}

// TestDiskMaintenanceInstalledCommitMustBeNamed: the plan refuses to rank tags
// against a commit `gt version` did not name, and returns the one it did.
func TestDiskMaintenanceInstalledCommitMustBeNamed(t *testing.T) {
	t.Parallel()
	if _, err := diskMaintInstalled(func() string { return "" })(); err == nil {
		t.Fatal("an unidentified installed commit read as a commit")
	}
	got, err := diskMaintInstalled(func() string { return "0123456789abcdef" })()
	if err != nil || got != "0123456789abcdef" {
		t.Fatalf("installed commit = %q, %v; want the commit the reader named", got, err)
	}
}

// TestDiskMaintenanceRunnerNamesTheFailure: a failed kubectl or docker is an
// error carrying the program and its last stderr line, never the whole output.
func TestDiskMaintenanceRunnerNamesTheFailure(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) {
		return nil, []byte("noise from a wrapper\nno such pod\n"), errors.New("exit status 1")
	}

	_, err := d.diskMaintRunner(context.Background(), nil, "kubectl", "get", "pods")
	if err == nil {
		t.Fatal("a failed program read as a success")
	}
	if !strings.Contains(err.Error(), "kubectl") || !strings.Contains(err.Error(), "no such pod") {
		t.Errorf("error = %v, want the program and its last stderr line", err)
	}
	if strings.Contains(err.Error(), "noise from a wrapper") {
		t.Errorf("error = %v, want the last stderr line only", err)
	}

	d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) { return []byte("ok"), nil, nil }
	out, err := d.diskMaintRunner(context.Background(), []string{"A=B"}, "kubectl", "get", "pods")
	if err != nil || string(out) != "ok" {
		t.Errorf("runner = %q, %v; want the program's stdout", out, err)
	}
}

// TestDiskMaintenancePushGateFollowsTheLock: the registry target's two answers
// come from the lock it actually holds — "no push is writing" only while it
// does — and a lock another run holds is the target's deferral, not a delete.
func TestDiskMaintenancePushGateFollowsTheLock(t *testing.T) {
	t.Parallel()
	cfg := polecatimage.Config{
		LockDir:    filepath.Join(t.TempDir(), "polecat-image.lock"),
		StaleAfter: 20 * time.Minute,
		PID:        os.Getpid(),
	}
	gate := &diskMaintPushGate{cfg: cfg, out: io.Discard, now: func() time.Time { return diskMaintNow }}

	if !gate.pushInFlight() {
		t.Error("pushInFlight outside the lock = false: only the lock's holder may say no push is writing")
	}

	var inside bool
	err := gate.withLock(context.Background(), func(context.Context) error {
		inside = !gate.pushInFlight()
		return nil
	})
	if err != nil {
		t.Fatalf("withLock = %v, want the body to run under the lock", err)
	}
	if !inside {
		t.Error("pushInFlight under the lock = true: the garbage collect would be skipped every time")
	}
	if !gate.pushInFlight() {
		t.Error("pushInFlight after the release = false: a push may hold the registry again")
	}

	// A second run holding the lock is the deferral the apply reports.
	err = polecatimage.WithLock(context.Background(), cfg, polecatimage.WithLockOptions{Out: io.Discard}, func(ctx context.Context) error {
		inner := gate.withLock(ctx, func(context.Context) error {
			t.Error("the deletes ran while another run held the lock")
			return nil
		})
		if !errors.Is(inner, registry.ErrDeferred) {
			t.Errorf("withLock with the plugin lock held = %v, want ErrDeferred", inner)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the outer lock failed: %v", err)
	}
}

// TestDiskMaintKubeconfigInContainerMode pins which kubeconfig the patrol reads
// the sandbox cluster through: the container's derived file in container mode,
// the host's file anywhere else. The host's file names 127.0.0.1, which the
// container cannot reach.
func TestDiskMaintKubeconfigInContainerMode(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name       string
		supervisor string
		want       string
	}{
		{"container mode", agentconfig.SupervisorContainer, filepath.Join(town, ".runtime", "container", "kubeconfig.yaml")},
		{"host, unset", "", filepath.Join(town, ".runtime", "sbx-k8s", "kubeconfig")},
		{"launchd owner", agentconfig.SupervisorLaunchd, filepath.Join(town, ".runtime", "sbx-k8s", "kubeconfig")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string {
				if key == agentconfig.ContainerModeEnv {
					return tt.supervisor
				}
				return ""
			}
			if got := diskMaintKubeconfig(town, getenv); got != tt.want {
				t.Errorf("diskMaintKubeconfig = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDiskMaintenanceRegistryTargetUsesTheHelpersKubeconfig pins that the
// registry target's in-use read names the cluster through the same kubeconfig
// the helper picks, so the registry and the cache trim agree on which cluster
// they see.
func TestDiskMaintenanceRegistryTargetUsesTheHelpersKubeconfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name       string
		supervisor string
		want       string
	}{
		{"container mode", agentconfig.SupervisorContainer, filepath.Join(town, ".runtime", "container", "kubeconfig.yaml")},
		{"host", "", filepath.Join(town, ".runtime", "sbx-k8s", "kubeconfig")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string {
				switch key {
				case agentconfig.ContainerModeEnv:
					return tt.supervisor
				case "POLECAT_IMAGE_REGISTRY":
					return "127.0.0.1:1"
				}
				return ""
			}
			cfg, err := polecatimage.ConfigFromEnv(func(key string) string {
				if key == "POLECAT_IMAGE_TOWN_ROOT" {
					return town
				}
				return getenv(key)
			})
			if err != nil {
				t.Fatalf("reading the plugin config: %v", err)
			}
			d := &Daemon{config: &Config{TownRoot: town}}
			regCfg, err := d.diskMaintRegistryConfig(agentconfig.DiskMaintenanceModeDryRun, cfg, t.TempDir(), getenv)
			if err != nil {
				t.Fatalf("building the registry config: %v", err)
			}
			if regCfg.Kubeconfig != tt.want {
				t.Errorf("registry target Kubeconfig = %q, want %q", regCfg.Kubeconfig, tt.want)
			}
		})
	}
}

// TestDiskMaintContainerModeRegistryCollectUnavailable pins that the container's
// registry target records its garbage collect as skipped: the container has no
// engine socket to the registry, and the Mac's registry-collect job collects
// instead. Outside container mode the target keeps its collect.
func TestDiskMaintContainerModeRegistryCollectUnavailable(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name       string
		supervisor string
		want       string
	}{
		{"container mode", agentconfig.SupervisorContainer, diskMaintCollectUnavailable},
		{"host, unset", "", ""},
		{"launchd owner", agentconfig.SupervisorLaunchd, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string {
				switch key {
				case agentconfig.ContainerModeEnv:
					return tt.supervisor
				case "POLECAT_IMAGE_REGISTRY":
					return "127.0.0.1:1"
				}
				return ""
			}
			cfg, err := polecatimage.ConfigFromEnv(func(key string) string {
				if key == "POLECAT_IMAGE_TOWN_ROOT" {
					return town
				}
				return getenv(key)
			})
			if err != nil {
				t.Fatalf("reading the plugin config: %v", err)
			}
			d := &Daemon{config: &Config{TownRoot: town}}
			regCfg, err := d.diskMaintRegistryConfig(agentconfig.DiskMaintenanceModeDryRun, cfg, t.TempDir(), getenv)
			if err != nil {
				t.Fatalf("building the registry config: %v", err)
			}
			if regCfg.CollectUnavailable != tt.want {
				t.Errorf("registry target CollectUnavailable = %q, want %q", regCfg.CollectUnavailable, tt.want)
			}
		})
	}
}

// TestDiskMaintCollectMessage pins that the container's skipped-collect reason
// does not name the Mac's registry-collect job, which was never installed.
func TestDiskMaintCollectMessage(t *testing.T) {
	t.Parallel()
	if strings.Contains(diskMaintCollectUnavailable, "registry-collect") {
		t.Errorf("collect message %q names the registry-collect job, which no job runs", diskMaintCollectUnavailable)
	}
}
