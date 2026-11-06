package daemon

// disk_maintenance is the town's registry prune: in the configured quiet window
// it plans every maintenance target and, when the mode is apply, applies the
// plan under the polecat-image lock so a delete never races an image push
// (gt-dl4nv.15).
//
// A target may set a cadence of its own, and the CI cache volumes do: they grow
// faster than a daily window can hold, so civol runs on its interval
// (agentconfig.DiskMaintenanceDefaultCivolInterval by default) in any window
// while the rest keep the daily pass (gt-z6qre.17). The shared Go cache trim is
// on the same footing: it runs on cache_trim_interval (2h by default) in any
// window, not in the daily pass (gt-gg9hv.24.2).
//
// The patrol is opt-in twice over. It runs only when
// daemon.patrols.disk_maintenance.enabled is true, and it changes nothing
// unless mode is "apply": the default is a dry run that logs what a prune would
// delete. A plan that could not read its inputs refuses rather than guessing.
//
// The disk readings, the 80/70 usage alerts and the unreadable alert are in
// disk_maintenance_disks.go; this file owns the schedule, the mode, and the
// pass that plans with the keep those readings decide.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// diskMaintCheckInterval is how often the daemon asks whether the window is
	// open. Five minutes catches a one-hour window at every tick without
	// waking the loop for anything else.
	diskMaintCheckInterval = 5 * time.Minute

	// diskMaintRunTimeout bounds one pass: the reads and deletes are HTTP and
	// one pod listing against the town's own registry, so a pass still running
	// after five minutes is stuck rather than slow.
	diskMaintRunTimeout = 5 * time.Minute

	// diskMaintDefaultKeep is the newest-commit window a pass plans with when
	// no tighter one is decided. It matches the plugin's own registry keep, so
	// a dry run shows what the plugin's prune would delete.
	diskMaintDefaultKeep = 5

	// diskMaintLedgerMaxBytes bounds the ledger before it rotates. A pass
	// appends one short line per target, so a mebibyte holds thousands.
	diskMaintLedgerMaxBytes = 1 << 20

	// diskMaintRegistryContainer is the registry container garbage collect
	// execs into; deploy/k8s/kind/up.sh creates it.
	diskMaintRegistryContainer = "gt-registry"

	// diskMaintKeepEnv is the keep knob gt maint and the plugin read.
	diskMaintKeepEnv = "POLECAT_IMAGE_REGISTRY_KEEP"

	// diskMaintCacheTrimName is the schedule key the cache trim's last run is
	// recorded under. It is not a target name, so it cannot collide with one.
	diskMaintCacheTrimName = "cache_trim"

	// diskMaintSandboxContext is the context the kind bootstrap generates for
	// cluster gt-sbx (deploy/k8s/kind, .runtime/sbx-k8s/kubeconfig). Every
	// cache trim call names it, so no call relies on the kubeconfig's
	// current-context, which the container's derived file does not carry
	// (gt-gg9hv.24.6).
	diskMaintSandboxContext = diskmaint.SandboxContext

	// cacheTrimRetryInterval is when a cache trim that could not reach a
	// verdict is tried again. A failed trim records this rather than the full
	// interval, so a transient refusal costs the next few five-minute ticks
	// instead of two hours (gt-gg9hv.24.6).
	cacheTrimRetryInterval = 15 * time.Minute
)

// diskMaintSeams are the patrol's outside reads and side effects, replaceable
// in tests through Daemon.diskMaint. Each nil field is the production
// behavior (see diskMaintenanceSeams).
type diskMaintSeams struct {
	// now is the clock the window and the once-per-day rule are read by.
	now func() time.Time
	// targets builds the maintenance targets a pass plans; nil is the town's
	// registry, built the way gt maint builds it.
	targets func(ctx context.Context, d *Daemon, mode string) (map[string]diskmaint.Target, error)
	// readDisks reads the disks the alerts and the keep tightening watch; nil
	// is the production read through dfReaders.
	readDisks func(ctx context.Context) []disks.Reading
	// dfReaders replaces the production df command of each named disk, for a
	// test that exercises the production read without launching a program.
	dfReaders map[string]DFReader
	// getenv reads the plugin settings the target is built from.
	getenv func(string) string
	// environ is the daemon's own environment, the source of the few
	// variables the cache trim's kubectl runs with (see cacheTrimKubectlEnv);
	// nil reads the process environment.
	environ func() []string
	// sandboxes reads the town's sandbox profiles, which name the shared cache
	// claims the pass trims and the image each trim Job runs; nil reads the
	// town's settings.
	sandboxes func() (map[string]agentconfig.SandboxProfile, error)
	// homeDir locates the colima docker socket's home.
	homeDir func() (string, error)
	// dispatch runs a pass off the calling goroutine, so a slow registry does
	// not hold the heartbeat. Nil means one goroutine per pass.
	dispatch func(fn func())
}

// diskMaintenanceSeams returns d's patrol seams with every unset one filled in
// with its production behavior.
func (d *Daemon) diskMaintenanceSeams() diskMaintSeams {
	s := d.diskMaint
	if s.now == nil {
		s.now = func() time.Time { return d.clk().Now() }
	}
	if s.targets == nil {
		s.targets = func(_ context.Context, d *Daemon, mode string) (map[string]diskmaint.Target, error) {
			return d.diskMaintenanceTargets(mode, s.getenv, s.homeDir)
		}
	}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.readDisks == nil {
		s.readDisks = func(ctx context.Context) []disks.Reading {
			return d.diskMaintenanceReadings(ctx, s.now(), s.dfReaders,
				diskMaintKubeconfig(d.config.TownRoot, s.getenv))
		}
	}
	if s.environ == nil {
		s.environ = os.Environ
	}
	if s.sandboxes == nil {
		s.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
			ts, err := agentconfig.LoadTownSettings(agentconfig.TownSettingsPath(d.config.TownRoot))
			if err != nil || ts == nil {
				return nil, err
			}
			return ts.Sandboxes, nil
		}
	}
	if s.homeDir == nil {
		s.homeDir = os.UserHomeDir
	}
	if s.dispatch == nil {
		s.dispatch = func(fn func()) { go fn() }
	}
	return s
}

// diskMaintenanceSettings returns the patrol's config block, or nil when the
// town declares none.
func diskMaintenanceSettings(cfg *DaemonPatrolConfig) *DiskMaintenanceConfig {
	if cfg == nil || cfg.Patrols == nil {
		return nil
	}
	return cfg.Patrols.DiskMaintenance
}

// diskMaintenanceWindow returns the HH:MM start of the patrol's quiet window.
// An empty setting is the default window rather than no window: a town that
// enabled the patrol without repeating the default still gets one.
func diskMaintenanceWindow(cfg *DaemonPatrolConfig) string {
	if c := diskMaintenanceSettings(cfg); c != nil && c.Window != "" {
		return c.Window
	}
	return agentconfig.DiskMaintenanceDefaultWindow
}

// diskMaintenanceCacheTrimEvery returns the cache trim's cadence: the default
// when the block or its setting is absent, and the refusal for a setting the
// patrol cannot schedule.
func diskMaintenanceCacheTrimEvery(cfg *DaemonPatrolConfig) (time.Duration, error) {
	var settings agentconfig.DiskMaintenanceConfig
	if c := diskMaintenanceSettings(cfg); c != nil {
		settings = *c
	}
	return settings.CacheTrimEvery()
}

// diskMaintenanceMode returns the pass mode, or the refusal for a mode the
// patrol cannot act on. Decoding refuses one too; this catches a config built
// in code, where the default must not silently become an apply.
func diskMaintenanceMode(cfg *DaemonPatrolConfig) (string, error) {
	raw := ""
	if c := diskMaintenanceSettings(cfg); c != nil {
		raw = c.Mode
	}
	switch raw {
	case "":
		return agentconfig.DiskMaintenanceModeDryRun, nil
	case agentconfig.DiskMaintenanceModeDryRun, agentconfig.DiskMaintenanceModeApply:
		return raw, nil
	}
	return "", fmt.Errorf("patrols.disk_maintenance.mode = %q: use %q or %q",
		raw, agentconfig.DiskMaintenanceModeDryRun, agentconfig.DiskMaintenanceModeApply)
}

// diskMaintenanceModeOrDefault is the mode for a log line, where a config the
// patrol would refuse reads as "invalid" rather than failing the line.
func diskMaintenanceModeOrDefault(cfg *DaemonPatrolConfig) string {
	mode, err := diskMaintenanceMode(cfg)
	if err != nil {
		return "invalid"
	}
	return mode
}

// diskMaintSchedule is the patrol's state between ticks: when each target with
// a cadence of its own last concluded a run, when a failed run asks for a
// retry, the lines already said, and the settings the patrol has already
// refused to schedule. All of it is in memory, as the daily window's own
// marker is, so a restarted daemon runs each target once rather than
// inheriting a deadline it never met.
type diskMaintSchedule struct {
	mu      sync.Mutex
	lastRun map[string]time.Time
	retryAt map[string]time.Time
	logged  map[string]time.Time
	refused map[string]bool
}

// lastRunAt is when the named target last concluded a run; the zero time when
// it has none, which reads as due.
func (s *diskMaintSchedule) lastRunAt(name string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRun[name]
}

// record keeps the time a target concluded a run. The reading loses its
// monotonic side, as a persisted one would: the seam clock carries none, and a
// due-ness subtraction across the two would count awake time only, so a host
// that slept would keep a target waiting past its interval.
func (s *diskMaintSchedule) record(name string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRun == nil {
		s.lastRun = make(map[string]time.Time)
	}
	s.lastRun[name] = at.Round(0)
	// A verdict replaces a failed run's retry: the interval runs from now.
	delete(s.retryAt, name)
}

// retryAtOf is when a failed run asked to be tried again; the zero time when
// it asked for none.
func (s *diskMaintSchedule) retryAtOf(name string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retryAt[name]
}

// cacheTrimDue reports whether the cache trim is due at now: a failed run's
// retry comes first, and otherwise the interval runs from the last verdict.
func (s *diskMaintSchedule) cacheTrimDue(now time.Time, every time.Duration) bool {
	if retry := s.retryAtOf(diskMaintCacheTrimName); !retry.IsZero() {
		return !now.Before(retry)
	}
	last := s.lastRunAt(diskMaintCacheTrimName)
	return last.IsZero() || now.Sub(last) >= every
}

// recordRetry keeps the time a failed run asks to be tried again. The last
// verdict is left as it was: if this failure clears, the interval still counts
// from the last run that concluded.
func (s *diskMaintSchedule) recordRetry(name string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retryAt == nil {
		s.retryAt = make(map[string]time.Time)
	}
	s.retryAt[name] = at.Round(0)
}

// allowLog reports whether the line key may be logged at now, and remembers
// it: the same line is said once per window, so a trim that fails on every
// retry is one line an hour rather than one every fifteen minutes. Entries
// past the window are dropped, so the set holds only the lines it still
// suppresses.
func (s *diskMaintSchedule) allowLog(key string, now time.Time, window time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logged == nil {
		s.logged = make(map[string]time.Time)
	}
	if last, ok := s.logged[key]; ok && now.Sub(last) < window {
		return false
	}
	for k, at := range s.logged {
		if now.Sub(at) >= window {
			delete(s.logged, k)
		}
	}
	s.logged[key] = now.Round(0)
	return true
}

// firstRefusal reports whether the patrol has yet to report the refusal key
// names, and marks it reported: a setting the patrol cannot schedule is worth
// one line, not one on every five-minute tick.
func (s *diskMaintSchedule) firstRefusal(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused[key] {
		return false
	}
	if s.refused == nil {
		s.refused = make(map[string]bool)
	}
	s.refused[key] = true
	return true
}

// diskMaintDue is the work one tick found: the targets with a cadence of their
// own that are due now, and whether the daily window's targets are due.
type diskMaintDue struct {
	// interval is the due target names with a cadence of their own, sorted.
	interval []string
	// scheduled is every target name given a cadence, due or not: it runs on
	// that cadence and never on the daily window.
	scheduled map[string]bool
	// daily is whether the targets without a cadence are due: the window is
	// open and the day's pass has not concluded.
	daily bool
	// cacheTrim is whether the cache trim's interval is due. It runs in any
	// window, and the daily window does not run it.
	cacheTrim bool
}

// empty reports whether this tick has nothing to run.
func (due diskMaintDue) empty() bool {
	return !due.daily && !due.cacheTrim && len(due.interval) == 0
}

// selection returns the built targets this tick runs, sorted. A target with a
// cadence runs only when the cadence is due; the daily window never reaches it.
func (due diskMaintDue) selection(targets map[string]diskmaint.Target) []string {
	names := make([]string, 0, len(targets))
	for name := range targets {
		if due.daily && !due.scheduled[name] {
			names = append(names, name)
		}
	}
	for _, name := range due.interval {
		if _, built := targets[name]; built {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// diskMaintenanceDue decides what this tick runs. A target with its own cadence
// is due from its own last run and ignores the window; a target without one
// keeps the window and its one run a day (gt-z6qre.17).
//
// The errors are settings the patrol cannot act on: a target name it does not
// know, or a cadence it cannot schedule. Decoding refuses them from the config
// file, and this is the same refusal for a config built in code: the target
// runs nothing rather than quietly on a clock the town never asked for
// (gt-kdbzb).
func (d *Daemon) diskMaintenanceDue(now time.Time) (diskMaintDue, []error) {
	due := diskMaintDue{scheduled: map[string]bool{}}
	var errs []error
	if cfg := diskMaintenanceSettings(d.patrolConfig); cfg != nil {
		for _, name := range slices.Sorted(maps.Keys(cfg.Targets)) {
			// A target given a cadence never runs on the daily window, whether
			// that cadence is due or unreadable: falling back would run it on a
			// clock the town never asked for.
			if cfg.Targets[name].Interval != "" {
				due.scheduled[name] = true
			}
			every, err := cfg.Cadence(name)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if every == 0 {
				continue // no cadence: the daily window decides
			}
			if last := d.diskMaintSchedule.lastRunAt(name); last.IsZero() || now.Sub(last) >= every {
				due.interval = append(due.interval, name)
			}
		}
	}
	// The cache trim is due from its own last run in any window, or from a
	// failed run's retry. A malformed setting is refused and the trim runs
	// nothing, as a target's would (gt-gg9hv.24.2).
	if every, err := diskMaintenanceCacheTrimEvery(d.patrolConfig); err != nil {
		errs = append(errs, err)
	} else if d.diskMaintSchedule.cacheTrimDue(now, every) {
		due.cacheTrim = true
	}
	if !shouldRunMaintenanceCycle(now, d.diskMaintenanceSettledAt()) {
		return due, errs
	}
	window := diskMaintenanceWindow(d.patrolConfig)
	if isInMaintenanceWindow(now, window) {
		due.daily = true
		return due, errs
	}
	if _, _, err := maintenanceWindowBounds(now, window); err != nil {
		// A window that does not parse never opens, so the patrol would
		// otherwise be a silent skip forever.
		errs = append(errs, err)
	}
	return due, errs
}

// runDiskMaintenance is the tick: it finds the targets due, refuses quietly
// while the town is busy, and then runs them off the loop goroutine.
//
// It runs on the loop goroutine and only reads the schedule: the pass writes
// it when it concludes a target, and a pass that deferred leaves that target
// due so a later tick tries again.
func (d *Daemon) runDiskMaintenance() {
	if !d.isPatrolActive("disk_maintenance") {
		return
	}
	s := d.diskMaintenanceSeams()
	now := s.now()

	due, errs := d.diskMaintenanceDue(now)
	for _, err := range errs {
		d.refuseDiskMaintenance(err)
	}
	if due.empty() {
		return
	}

	mode, err := diskMaintenanceMode(d.patrolConfig)
	if err != nil {
		// A mode the patrol cannot act on runs nothing. The mode is checked again
		// on every tick that has work due; the refusal is reported once rather
		// than on each of them, and the schedule stands.
		d.refuseDiskMaintenance(err)
		return
	}

	quiet, why := d.maintenance().quiet(d)
	if !quiet {
		d.runDiskMaintenanceTrim(mode, due, why)
		return
	}
	if !d.diskMaintRunning.CompareAndSwap(false, true) {
		return // a pass is still running
	}
	s.dispatch(func() {
		defer d.diskMaintRunning.Store(false)
		d.diskMaintenancePass(now, mode, due)
	})
}

// runDiskMaintenanceTrim is the tick's busy case: the cache trim may run, the
// targets may not. The trim is off the Dolt window's quiet guard so continuous
// landing passes cannot starve it (gt-gg9hv.48; docs/guides/disk-maintenance.md).
// Everything else the tick found keeps the guard: it stays due, and the next
// quiet tick runs it.
func (d *Daemon) runDiskMaintenanceTrim(mode string, due diskMaintDue, why string) {
	if !due.cacheTrim {
		d.logger.Printf("disk_maintenance: skipped: %s", why)
		return
	}
	if due.daily || len(due.interval) > 0 {
		// The trim runs on this tick; the targets it found are what this line
		// is about, so their deferral is not read as the tick doing nothing.
		d.logger.Printf("disk_maintenance: targets skipped: %s; the cache trim runs on its own pass", why)
	}
	if !d.diskMaintTrimRunning.CompareAndSwap(false, true) {
		return // a trim pass is still running
	}
	d.diskMaintenanceSeams().dispatch(func() {
		defer d.diskMaintTrimRunning.Store(false)
		d.runCacheTrimPass(mode)
	})
}

// runCacheTrimPass is the trim's own pass, the one a busy tick dispatches: the
// disk readings and their alerts first, as the combined pass raises them, then
// the trim. Its caller holds the trim's guard (see runDiskMaintenanceTrim),
// which is the trim's own and not the target pass's, so a long target pass
// does not keep it waiting.
func (d *Daemon) runCacheTrimPass(mode string) {
	ctx, cancel := d.diskMaintenanceContext()
	defer cancel()
	s := d.diskMaintenanceSeams()
	d.applyDiskAlerts(ctx, s.readDisks, s.now())
	d.diskMaintenanceTrim(ctx, mode)
}

// runCacheTrimInPass is the trim inside the combined pass: it tries the trim's
// single-flight guard and, when it wins, runs the trim under it. It reports
// whether this call ran the trim: one already in flight is left to finish, and
// two trim passes never overlap.
func (d *Daemon) runCacheTrimInPass(ctx context.Context, mode string) bool {
	if !d.diskMaintTrimRunning.CompareAndSwap(false, true) {
		return false
	}
	defer d.diskMaintTrimRunning.Store(false)
	d.diskMaintenanceTrim(ctx, mode)
	return true
}

// diskMaintenanceTrim is the trim and its schedule record: the full interval
// once it reached a verdict, the short retry when it could not start or
// conclude. The caller holds the trim's guard.
func (d *Daemon) diskMaintenanceTrim(ctx context.Context, mode string) {
	s := d.diskMaintenanceSeams()
	if d.cacheTrim(ctx, mode) {
		d.diskMaintSchedule.record(diskMaintCacheTrimName, s.now())
	} else {
		d.diskMaintSchedule.recordRetry(diskMaintCacheTrimName, s.now().Add(cacheTrimRetryInterval))
	}
}

// refuseDiskMaintenance says why a setting the patrol cannot schedule runs
// nothing, once rather than on every tick.
func (d *Daemon) refuseDiskMaintenance(err error) {
	if d.diskMaintSchedule.firstRefusal(err.Error()) {
		d.logger.Printf("disk_maintenance: not running: %v", err)
	}
}

// settleDiskMaintenanceWindow records that this window's pass is over, so the
// next tick does not repeat it; the once-a-day gap is measured from here.
func (d *Daemon) settleDiskMaintenanceWindow(now time.Time) {
	d.diskMaintSettled.Store(now.UnixNano())
}

// diskMaintenanceSettledAt is when the patrol last settled a window; the zero
// time when it has none, which reads as due.
func (d *Daemon) diskMaintenanceSettledAt() time.Time {
	if at := d.diskMaintSettled.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}

// diskMaintenancePass plans the due targets and, in apply mode, applies the
// plans it just built. A target is settled by anything its run reaches a
// conclusion about — a kept plan, a refused one, a failed read — and left due
// when an apply deferred, so the next tick retries once the push holding the
// lock is done.
func (d *Daemon) diskMaintenancePass(now time.Time, mode string, due diskMaintDue) {
	ctx, cancel := d.diskMaintenanceContext()
	defer cancel()
	s := d.diskMaintenanceSeams()

	// The disk alerts are an alarm rather than a change, so they are read and
	// raised before anything that can refuse the plan: a disk too full to keep
	// pruning, or a keep the patrol cannot decide, must not silence them.
	readings := d.applyDiskAlerts(ctx, s.readDisks, now)

	// The cache trim is independent of the targets: it trims the claim the
	// sandbox profiles mount, so it runs when its interval is due whether or not
	// a maintenance target publishes here (gt-rm4am, gt-gg9hv.24.2). It holds
	// its own guard, so a trim pass already in flight is left to finish rather
	// than trimmed twice, and this pass's targets do not keep the next trim
	// waiting (gt-gg9hv.48).
	if due.cacheTrim {
		d.runCacheTrimInPass(ctx, mode)
	}
	if !due.daily && len(due.interval) == 0 {
		return // only the cache trim was due, and it has run
	}

	keep, err := diskMaintenanceKeep(s.getenv, readings)
	if err != nil {
		d.logger.Printf("disk_maintenance: not running: %v", err)
		d.concludeDiskMaintenance(now, due)
		return
	}
	targets, err := s.targets(ctx, d, mode)
	if err != nil {
		d.logger.Printf("disk_maintenance: the targets could not be built: %v", err)
		d.concludeDiskMaintenance(now, due)
		return
	}
	if len(targets) == 0 {
		d.logger.Printf("disk_maintenance: no maintenance target publishes here; nothing to do")
		d.concludeDiskMaintenance(now, due)
		return
	}
	for _, name := range due.interval {
		if _, built := targets[name]; built {
			continue
		}
		// A cadence set for a target this build does not produce runs nothing.
		// Saying so once keeps it from looking like a run that keeps failing,
		// and recording it means the patrol looks again per interval, not per
		// tick.
		if d.diskMaintSchedule.firstRefusal("absent:" + name) {
			d.logger.Printf("disk_maintenance: %s: this build has no such target; its interval runs nothing", name)
		}
		d.diskMaintSchedule.record(name, s.now())
	}

	apply := mode == agentconfig.DiskMaintenanceModeApply
	policy := diskmaint.Policy{Keep: keep, Apply: apply, Now: now}
	deferred := false
	for _, name := range due.selection(targets) {
		if d.diskMaintenanceTarget(ctx, name, targets[name], policy, apply) {
			deferred = true
			continue
		}
		if due.scheduled[name] {
			d.diskMaintSchedule.record(name, s.now())
		}
	}
	if deferred {
		d.logger.Printf("disk_maintenance: apply deferred; the window stays open for another try")
		return
	}
	if due.daily {
		d.settleDiskMaintenanceWindow(now)
	}
}

// concludeDiskMaintenance settles the targets due this tick without running
// them, for a pass that refused whole: a failed read concludes a run as a plan
// does, and retrying it on every tick would be a line and a build apiece.
func (d *Daemon) concludeDiskMaintenance(now time.Time, due diskMaintDue) {
	if due.daily {
		d.settleDiskMaintenanceWindow(now)
	}
	for _, name := range due.interval {
		d.diskMaintSchedule.record(name, d.diskMaintenanceSeams().now())
	}
}

// diskMaintenanceTarget plans one target and, in apply mode, applies the plan.
// It reports whether the apply deferred.
func (d *Daemon) diskMaintenanceTarget(ctx context.Context, name string, target diskmaint.Target, policy diskmaint.Policy, apply bool) bool {
	plan, err := target.Plan(ctx, policy)
	if err != nil {
		d.logger.Printf("disk_maintenance: %s: the plan could not be built: %v", name, err)
		return false
	}
	if plan.Refused != "" {
		// A refused plan is an alarm in both modes: it means the run could not
		// check its inputs, not that the registry was clean.
		d.logger.Printf("disk_maintenance: %s: refused: %s", name, plan.Refused)
		return false
	}
	if !apply {
		d.logger.Printf("disk_maintenance: %s dry_run: %d to delete, %d kept", name, len(plan.Actions), len(plan.Kept))
		return false
	}
	result, err := target.Apply(ctx, plan)
	switch {
	case result.Deferred != "":
		d.logger.Printf("disk_maintenance: %s apply deferred: %s", name, result.Deferred)
		return true
	case err != nil:
		d.logger.Printf("disk_maintenance: %s apply failed: %v (%d deleted, %d failed)", name, err, len(result.Done), len(result.Failed))
	default:
		d.logger.Printf("disk_maintenance: %s apply: deleted %d of %d, freed %d bytes",
			name, len(result.Done), len(plan.Actions), result.BytesFreed)
	}
	return false
}

// diskMaintenanceContext bounds one pass and stops its reads when the daemon
// shuts down. The caller must cancel it: that is what releases the timer.
func (d *Daemon) diskMaintenanceContext() (context.Context, context.CancelFunc) {
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, diskMaintRunTimeout)
}

// diskMaintenanceKeep returns the keep count this pass plans with: the
// configured window, tightened when a disk is nearly full. A disk that could
// not be read refuses the count, because a keep decided without it would be a
// guess (disks.KeepFor); the refusal is reported and no count is applied.
func diskMaintenanceKeep(getenv func(string) string, readings []disks.Reading) (int, error) {
	base, err := diskMaintKeep(getenv)
	if err != nil {
		return 0, err
	}
	keep, err := disks.KeepFor(readings, base).Count()
	if err != nil {
		return 0, fmt.Errorf("refusing the plan: %w", err)
	}
	return keep, nil
}

// diskMaintKeep reads the keep window the registry prune uses. A value that is
// not a whole number of images is an error rather than the default, because a
// plan built on a different window would delete what the plugin keeps.
func diskMaintKeep(get func(string) string) (int, error) {
	value := get(diskMaintKeepEnv)
	if value == "" {
		return diskMaintDefaultKeep, nil
	}
	keep, err := strconv.Atoi(value)
	if err != nil || keep < 1 {
		return 0, fmt.Errorf("%s=%s is not a whole number of images to keep", diskMaintKeepEnv, value)
	}
	return keep, nil
}

// diskMaintenanceTargets builds the town's maintenance targets from the same
// settings the polecat-image plugin reads, so the patrol and the plugin agree
// on the registry, the lock and the ledger. A town that publishes nothing has
// no registry to prune and gets no target.
//
// gt maint builds this target too (maintTargets in internal/cmd/maint.go).
// internal/cmd imports this package, so the patrol cannot call it: change one
// builder and change the other.
//
// Only an apply may create the ledger directory: a dry run must leave the disk
// as it found it.
func (d *Daemon) diskMaintenanceTargets(mode string, getenv func(string) string, homeDir func() (string, error)) (map[string]diskmaint.Target, error) {
	town := d.config.TownRoot
	cfg, err := polecatimage.ConfigFromEnv(func(key string) string {
		if key == "POLECAT_IMAGE_TOWN_ROOT" {
			return town
		}
		return getenv(key)
	})
	if err != nil {
		return nil, err
	}
	if cfg.Registry == "" {
		return map[string]diskmaint.Target{}, nil
	}
	if strings.Contains(cfg.Registry, "/") {
		// The value once named the sandbox records directory, so a path here is
		// that old setting, and a URL is a scheme this reader adds itself.
		return nil, errors.New("POLECAT_IMAGE_REGISTRY is not a host:port; the registry address is host:port, and the sandbox records are POLECAT_IMAGE_SANDBOX_RECORDS")
	}
	home, err := homeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot locate the colima docker socket: %w", err)
	}
	regCfg, err := d.diskMaintRegistryConfig(mode, cfg, home, getenv)
	if err != nil {
		return nil, err
	}
	return map[string]diskmaint.Target{"registry": registry.NewTarget(regCfg)}, nil
}

// diskMaintRegistryConfig is the registry target's configuration. Its in-use
// read names the cluster through the kubeconfig diskMaintKubeconfig picks and
// the context every cluster call names, so the two readers agree on which
// cluster they see.
func (d *Daemon) diskMaintRegistryConfig(mode string, cfg polecatimage.Config, home string, getenv func(string) string) (registry.Config, error) {
	town := d.config.TownRoot
	ledgerDir := filepath.Join(town, ".runtime", "diskmaint")
	if mode == agentconfig.DiskMaintenanceModeApply {
		if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
			return registry.Config{}, fmt.Errorf("creating the maintenance ledger directory: %w", err)
		}
	}
	gate := &diskMaintPushGate{
		cfg: cfg,
		out: d.diskMaintenanceOut(),
		now: func() time.Time { return d.clk().Now() },
	}
	regCfg := registry.Config{
		Base:         "http://" + cfg.Registry,
		Kubeconfig:   diskMaintKubeconfig(town, getenv),
		Context:      diskMaintSandboxContext,
		RecordsDir:   cfg.SandboxRecords,
		DockerHost:   "unix://" + filepath.Join(home, ".colima", "gt-sbx", "docker.sock"),
		Container:    diskMaintRegistryContainer,
		Keep:         diskMaintDefaultKeep,
		Installed:    diskMaintInstalled(func() string { return (&polecatimage.Run{Cfg: cfg}).InstalledCommit() }),
		PushInFlight: gate.pushInFlight,
		Lock:         gate.withLock,
		Run:          d.diskMaintRunner,
		Ledger:       diskmaint.NewLedger(filepath.Join(ledgerDir, "ledger.jsonl"), diskMaintLedgerMaxBytes),
	}
	if containerMode(getenv) {
		regCfg.CollectUnavailable = diskMaintCollectUnavailable
	}
	return regCfg, nil
}

// diskMaintCollectUnavailable is why the container's registry target records a
// collect as skipped: the container has no engine socket to the registry, and
// the garbage collect runs from the Mac's registry-collect job instead.
const diskMaintCollectUnavailable = "the garbage collect runs from the Mac's registry-collect job, which this container cannot reach"

const (
	// diskMaintHostKubeconfig is the town's sandbox kubeconfig, relative to the
	// town root: the file the host reads the sandbox cluster through.
	diskMaintHostKubeconfig = ".runtime/sbx-k8s/kubeconfig"

	// diskMaintContainerKubeconfig is the kubeconfig the container derives for
	// the same cluster, relative to the town root. The host's file names
	// 127.0.0.1, which inside the container is the container's own loopback, so
	// kubectl is refused there. The writer is containerKubeconfigRP in
	// internal/cmd; that package cannot be imported here, so the path is kept
	// in step by hand.
	diskMaintContainerKubeconfig = ".runtime/container/kubeconfig.yaml"
)

// diskMaintKubeconfig is the kubeconfig the patrol reads the sandbox cluster
// through, under townRoot. In container mode (see containerMode) that is the
// container's derived file; anywhere else it is the host's.
func diskMaintKubeconfig(townRoot string, getenv func(string) string) string {
	rel := diskMaintHostKubeconfig
	if containerMode(getenv) {
		rel = diskMaintContainerKubeconfig
	}
	return filepath.Join(townRoot, filepath.FromSlash(rel))
}

// diskMaintenanceOut is where the plugin lock's log lines go.
func (d *Daemon) diskMaintenanceOut() io.Writer {
	if d.logger == nil {
		return io.Discard
	}
	return d.logger.Writer()
}

// diskMaintInstalled turns a `gt version` reader into the target's installed
// commit. A build that names none is an error, so the plan refuses rather than
// keeping tags for a commit it guessed.
func diskMaintInstalled(read func() string) func() (string, error) {
	return func() (string, error) {
		commit := read()
		if commit == "" {
			return "", errors.New("gt version names no commit")
		}
		return commit, nil
	}
}

// diskMaintRunner runs one program for a maintenance target through the
// daemon's exec seam, so the unit tier never launches kubectl, docker or gt. A
// failure carries the program's last stderr line, never its whole output.
func (d *Daemon) diskMaintRunner(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: a fixed program and a target's validated refs
	cmd.Env = append(os.Environ(), env...)
	stdout, stderr, err := d.runCmd(cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, lastLine(string(stderr)))
	}
	return stdout, nil
}

// diskMaintPushGate answers the registry target's two questions about the
// plugin lock. A push holds that lock, so possessing it proves no push is
// writing and the garbage collect may run; without it the answer is "a push
// may be" and the collect is skipped rather than guessed. The flag is atomic
// because the apply's delete loop reads it while the lock's callback writes it.
type diskMaintPushGate struct {
	cfg  polecatimage.Config
	out  io.Writer
	now  func() time.Time
	held atomic.Bool
}

// pushInFlight reports whether a push may hold the registry; only the holder
// of the lock can answer that no push does.
func (g *diskMaintPushGate) pushInFlight() bool { return !g.held.Load() }

// withLock runs fn under the plugin lock, reporting a held lock as the
// registry's deferral so the apply leaves the registry alone and says why.
//
// held tracks the lock itself, so it falls as soon as WithLock has released
// it: a flag left set after the release would answer "no push is writing" for
// every call the pass makes afterwards, including ones where a push has since
// started.
func (g *diskMaintPushGate) withLock(ctx context.Context, fn func(context.Context) error) error {
	err := polecatimage.WithLock(ctx, g.cfg, polecatimage.WithLockOptions{Out: g.out, Now: g.now}, func(ctx context.Context) error {
		g.held.Store(true)
		// The release is the defer's, not the next line's: the flag must fall
		// even when the body fails or panics, and it must fall before
		// WithLock's own Cleanup gives the lock away.
		defer g.held.Store(false)
		return fn(ctx)
	})
	if errors.Is(err, polecatimage.ErrLockHeld) {
		return fmt.Errorf("%w: %w", registry.ErrDeferred, err)
	}
	return err
}
