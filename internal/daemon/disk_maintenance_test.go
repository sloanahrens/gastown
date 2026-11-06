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
	if got := dm.Targets[agentconfig.DiskMaintenanceTargetCivol].Interval; got != "2h" {
		t.Errorf("civol interval = %q, want 2h: the CI cache volumes grow too fast for the daily window", got)
	}
	if len(dm.Targets) != 1 {
		t.Errorf("targets = %+v, want civol alone: every other target keeps the daily window", dm.Targets)
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
	if got := back.DiskMaintenance.Targets[agentconfig.DiskMaintenanceTargetCivol].Interval; got != "2h" {
		t.Errorf("civol interval decoded back as %q, want 2h", got)
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
	civol := &diskMaintFakeTarget{}
	f.extra = map[string]*diskMaintFakeTarget{"civol": civol}
	d.patrolConfig = diskMaintCadence("civol", "2h")
	now := diskMaintNow.Add(8 * time.Hour) // 12:45, well outside the 04:30 window
	d.diskMaint.now = func() time.Time { return now }

	d.runDiskMaintenance()
	if len(civol.policies) != 1 {
		t.Fatalf("civol planned %d times at 12:45, want 1: a 2h interval does not wait for the window", len(civol.policies))
	}
	if len(f.target.policies) != 0 {
		t.Errorf("registry planned %d times at 12:45, want 0: a target with no interval keeps the window", len(f.target.policies))
	}

	now = now.Add(2*time.Hour - time.Minute)
	d.runDiskMaintenance()
	if len(civol.policies) != 1 {
		t.Fatalf("civol planned %d times a minute before its interval was up, want 1", len(civol.policies))
	}

	now = now.Add(time.Minute)
	d.runDiskMaintenance()
	if len(civol.policies) != 2 {
		t.Fatalf("civol planned %d times once its interval was up, want 2", len(civol.policies))
	}
}

// TestDiskMaintenanceCadenceCountsFromTheConclusion: an interval counts from when
// the target concluded its last run, not from the tick that started it, so a
// pass that takes minutes does not shorten the wait before the next one.
func TestDiskMaintenanceCadenceCountsFromTheConclusion(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	civol := &diskMaintFakeTarget{}
	f.extra = map[string]*diskMaintFakeTarget{"civol": civol}
	d.patrolConfig = diskMaintCadence("civol", "2h")
	now := diskMaintNow.Add(8 * time.Hour) // 12:45
	d.diskMaint.now = func() time.Time { return now }
	civol.during = func() { now = now.Add(3 * time.Minute) } // the plan takes three minutes

	d.runDiskMaintenance()
	if len(civol.policies) != 1 {
		t.Fatalf("civol planned %d times at 12:45, want 1", len(civol.policies))
	}

	now = diskMaintNow.Add(10*time.Hour + time.Minute) // 14:46: two hours after the tick, not the conclusion
	civol.during = nil
	d.runDiskMaintenance()
	if len(civol.policies) != 1 {
		t.Fatalf("civol planned %d times at 14:46, want 1: the 2h interval runs from the 12:48 conclusion, not the 12:45 tick", len(civol.policies))
	}

	now = diskMaintNow.Add(10*time.Hour + 3*time.Minute) // 14:48: two hours after the conclusion
	d.runDiskMaintenance()
	if len(civol.policies) != 2 {
		t.Fatalf("civol planned %d times at 14:48, want 2: its interval was up at the 12:48 conclusion", len(civol.policies))
	}
}

// TestDiskMaintenanceCadenceLeavesTheDailyWindowToTheOthers: giving one target
// a cadence does not move the others onto it, and a cadence set for a target
// this build does not produce is said once and looked for per interval, not
// per tick.
func TestDiskMaintenanceCadenceLeavesTheDailyWindowToTheOthers(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintCadence("civol", "2h")
	now := diskMaintNow.Add(8 * time.Hour) // 12:45, outside the window
	d.diskMaint.now = func() time.Time { return now }

	d.runDiskMaintenance()

	if len(f.target.policies) != 0 {
		t.Fatalf("registry planned %d times at 12:45, want 0: it has no interval", len(f.target.policies))
	}
	if got := strings.Count(logs.String(), "has no such target"); got != 1 {
		t.Errorf("said the civol target is absent %d times, want 1: %q", got, logs.String())
	}

	now = diskMaintNow.Add(24*time.Hour + 15*time.Minute) // 05:00, inside the next window
	d.diskMaint.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		d.runDiskMaintenance()
	}
	if len(f.target.policies) != 1 {
		t.Fatalf("registry planned %d times in one window, want 1", len(f.target.policies))
	}
	if got := strings.Count(logs.String(), "has no such target"); got != 1 {
		t.Errorf("said the civol target is absent %d times, want 1: the report is not repeated per tick", got)
	}
}

// TestDiskMaintenanceCadenceSkipsWhenNotQuiet: a due interval target takes the
// same quiet gate as the daily pass, and a skipped tick does not spend the
// interval.
func TestDiskMaintenanceCadenceSkipsWhenNotQuiet(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	civol := &diskMaintFakeTarget{}
	f.extra = map[string]*diskMaintFakeTarget{"civol": civol}
	d.patrolConfig = diskMaintCadence("civol", "2h")
	d.diskMaint.now = func() time.Time { return diskMaintNow.Add(8 * time.Hour) }
	f.quiet, f.quietWhy = false, "slot held by gastown/polecats/corpus"

	d.runDiskMaintenance()

	if len(civol.policies) != 0 {
		t.Errorf("civol planned %d times while the town was busy, want 0", len(civol.policies))
	}
	if !strings.Contains(logs.String(), "slot held by gastown/polecats/corpus") {
		t.Errorf("log = %q, want the reason the run was skipped", logs.String())
	}

	f.quiet = true
	d.runDiskMaintenance()
	if len(civol.policies) != 1 {
		t.Errorf("civol planned %d times once the town went quiet, want 1: a skipped tick must not spend the interval", len(civol.policies))
	}
}

// TestDiskMaintenanceCadenceAppliesAndRetriesADeferral: a due interval target
// applies in apply mode, and an apply the plugin lock deferred leaves it due,
// so the next tick tries again rather than waiting out the interval.
func TestDiskMaintenanceCadenceAppliesAndRetriesADeferral(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	civol := &diskMaintFakeTarget{}
	f.extra = map[string]*diskMaintFakeTarget{"civol": civol}
	cfg := diskMaintCadence("civol", "2h")
	cfg.Patrols.DiskMaintenance.Mode = agentconfig.DiskMaintenanceModeApply
	d.patrolConfig = cfg
	now := diskMaintNow.Add(8 * time.Hour)
	d.diskMaint.now = func() time.Time { return now }
	civol.plan = diskmaint.Plan{Actions: []diskmaint.Action{{Kind: "delete-path", Ref: "ci-gocache/x"}}}
	civol.result = diskmaint.Result{Deferred: "polecat-image lock is held"}

	d.runDiskMaintenance()
	if len(civol.applied) != 1 {
		t.Fatalf("applied %d times, want 1: an interval target applies in apply mode", len(civol.applied))
	}
	if !strings.Contains(logs.String(), "deferred") {
		t.Errorf("log = %q, want the deferral named", logs.String())
	}

	civol.result = diskmaint.Result{}
	now = now.Add(5 * time.Minute)
	d.runDiskMaintenance()
	if len(civol.applied) != 2 {
		t.Fatalf("applied %d times, want 2: a deferral leaves the target due", len(civol.applied))
	}

	now = now.Add(5 * time.Minute)
	d.runDiskMaintenance()
	if len(civol.applied) != 2 {
		t.Fatalf("applied %d times, want 2: a completed run spends the interval", len(civol.applied))
	}
}

// countCacheTrims counts the trims a pass runs. A cache trim reads the sandbox
// profiles before anything else, so each read is one trim.
func countCacheTrims(d *Daemon) *int {
	trims := new(int)
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		*trims++
		return nil, nil
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

// TestDiskMaintCacheTrimRunsWhileALandingPassIsInFlight: the trim applies
// ConfigMaps and a Job on the sandbox cluster and reads neither the landing
// worker nor Dolt, so the Dolt window's quiet guard does not hold it off. A busy
// town with only the trim due reads the disks, raises their alerts, trims, and
// logs no skipped line for it (gt-gg9hv.48).
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

// TestDiskMaintAlertApplicationIsSerialised: the trim pass and the combined
// pass both apply the disk alerts, and they may run at the same time. One
// application at a time is what keeps the two from deciding from state the
// other is halfway through changing: a target pass that starts while the trim
// pass holds the alert application waits, and neither raise is lost
// (gt-gg9hv.48).
func TestDiskMaintAlertApplicationIsSerialised(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	gate := newGatedAlertNotifier(f.notify)
	d.notifier = gate
	// The trim itself reaches no claim, so the passes under test are their
	// alert applications and the decisions that follow.
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) { return nil, nil }

	// The town is busy for the trim's tick and quiet for the target's, so the
	// test drives both passes through the tick that dispatches each of them.
	var quiet atomic.Bool
	d.maint.quiet = func(*Daemon) (bool, string) {
		if quiet.Load() {
			return true, ""
		}
		return false, "daemon has work in flight (landing pass dv-x)"
	}
	var passes []func()
	d.diskMaint.dispatch = func(fn func()) { passes = append(passes, fn) }

	var reads atomic.Int32
	targetRead := make(chan struct{})
	d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		switch reads.Add(1) {
		case 1:
			return []disks.Reading{{Name: "mac", UsedPct: 85}}
		case 2:
			close(targetRead)
			return []disks.Reading{{Name: "colima", UsedPct: 88}}
		default:
			return nil
		}
	}

	d.runDiskMaintenance() // 04:45, busy: dispatches the trim pass alone
	if len(passes) != 1 {
		t.Fatalf("dispatched %d passes, want 1: the busy tick's trim pass", len(passes))
	}
	trimPass := passes[0]
	trimDone := make(chan struct{})
	go func() {
		defer close(trimDone)
		trimPass()
	}()
	<-gate.entered // the trim pass is inside its alert application, holding it

	quiet.Store(true)
	d.runDiskMaintenance() // 04:45, quiet: dispatches the combined pass
	if len(passes) != 2 {
		t.Fatalf("dispatched %d passes, want 2: the quiet tick's combined pass", len(passes))
	}
	targetPass := passes[1]
	targetDone := make(chan struct{})
	go func() {
		defer close(targetDone)
		targetPass()
	}()
	<-targetRead // the combined pass has read the disks and is at the same application
	close(gate.release)

	<-trimDone
	<-targetDone

	if got := gate.max.Load(); got != 1 {
		t.Errorf("%d alert applications ran at once, want 1: the trim pass and the target pass applied the alerts concurrently", got)
	}
	for _, key := range []string{"diskmaint:mac", "diskmaint:colima"} {
		if !diskMaintAlertIsOpen(f.notify, key) {
			t.Errorf("alert %s is not open: a raise was lost", key)
		}
	}
}

// gatedAlertNotifier holds the first escalation inside its delivery, so a test
// can keep one pass in the middle of its alert application, and counts the
// escalations inside at once: their applications were serialized if that never
// exceeds one.
type gatedAlertNotifier struct {
	notify.Notifier
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	inFlight atomic.Int32
	max      atomic.Int32
}

func newGatedAlertNotifier(inner notify.Notifier) *gatedAlertNotifier {
	return &gatedAlertNotifier{
		Notifier: inner,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (n *gatedAlertNotifier) Escalate(ctx context.Context, e notify.Escalation) error {
	cur := n.inFlight.Add(1)
	defer n.inFlight.Add(-1)
	for {
		if m := n.max.Load(); cur <= m || n.max.CompareAndSwap(m, cur) {
			break
		}
	}
	n.once.Do(func() {
		close(n.entered)
		<-n.release
	})
	return n.Notifier.Escalate(ctx, e)
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
	design := `{"disk_maintenance": {"enabled": true, "interval": "daily", "window": "04:30", "mode": "dry_run", "targets": {"civol": {"interval": "2h"}}}}`
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
	if got := dm.Targets[agentconfig.DiskMaintenanceTargetCivol].Interval; got != "2h" {
		t.Errorf("civol interval decoded as %q, want 2h", got)
	}

	bad := map[string]string{
		"unknown mode":       `{"disk_maintenance": {"enabled": true, "mode": "applyy"}}`,
		"unknown interval":   `{"disk_maintenance": {"interval": "hourly"}}`,
		"malformed window":   `{"disk_maintenance": {"window": "4:30pm"}}`,
		"unknown key":        `{"disk_maintenance": {"enable": true}}`,
		"unknown target":     `{"disk_maintenance": {"targets": {"go-build": {"interval": "2h"}}}}`,
		"malformed cadence":  `{"disk_maintenance": {"targets": {"civol": {"interval": "soon"}}}}`,
		"zero cadence":       `{"disk_maintenance": {"targets": {"civol": {"interval": "0s"}}}}`,
		"negative cadence":   `{"disk_maintenance": {"targets": {"civol": {"interval": "-2h"}}}}`,
		"sub-minute cadence": `{"disk_maintenance": {"targets": {"civol": {"interval": "30s"}}}}`,
		"unknown target key": `{"disk_maintenance": {"targets": {"civol": {"every": "2h"}}}}`,
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
