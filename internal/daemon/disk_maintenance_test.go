package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
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
}

func (f *diskMaintFakeTarget) Name() string { return "registry" }

func (f *diskMaintFakeTarget) Measure(context.Context) (diskmaint.Measurement, error) {
	return diskmaint.Measurement{}, nil
}

func (f *diskMaintFakeTarget) Plan(_ context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	f.policies = append(f.policies, p)
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
	target     *diskMaintFakeTarget
	targets    int
	targetsErr error
	readings   []disks.Reading
	env        map[string]string
	quiet      bool
	quietWhy   string
}

// diskMaintTestDaemon builds a daemon whose patrol runs inline, on a fixed
// clock, against a fake target. The window is the default one.
func diskMaintTestDaemon(t *testing.T) (*Daemon, *bytes.Buffer, *diskMaintFakes) {
	t.Helper()
	var buf bytes.Buffer
	f := &diskMaintFakes{target: &diskMaintFakeTarget{}, quiet: true}
	d := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(&buf, "", 0),
		patrolConfig: diskMaintEnabled("", ""),
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
		return map[string]diskmaint.Target{"registry": f.target}, nil
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
	design := `{"disk_maintenance": {"enabled": true, "interval": "daily", "window": "04:30", "mode": "dry_run"}}`
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

	bad := map[string]string{
		"unknown mode":     `{"disk_maintenance": {"enabled": true, "mode": "applyy"}}`,
		"unknown interval": `{"disk_maintenance": {"interval": "hourly"}}`,
		"malformed window": `{"disk_maintenance": {"window": "4:30pm"}}`,
		"unknown key":      `{"disk_maintenance": {"enable": true}}`,
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
	targets, err := d.diskMaintenanceTargets(agentconfig.DiskMaintenanceModeApply, getenv)
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
