package daemon

// disk_maintenance is the town's daily registry prune: in the configured quiet
// window it plans every maintenance target and, when the mode is apply, applies
// the plan under the polecat-image lock so a delete never races an image push
// (gt-dl4nv.15, docs/plans/2026-10-28-disk-maintenance-design.md).
//
// The patrol is opt-in twice over. It runs only when
// daemon.patrols.disk_maintenance.enabled is true, and it changes nothing
// unless mode is "apply": the default is a dry run that logs what a prune would
// delete. A plan that could not read its inputs refuses rather than guessing.
//
// Disk readings, the 80/70 alerts and the keep tightening arrive with
// gt-dl4nv.16 (disk_maintenance_disks.go); this file leaves the seam they fill.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	// readDisks reads the disks the keep tightening watches. The production
	// readers arrive with gt-dl4nv.16, so production reads none until then.
	readDisks func(ctx context.Context) []disks.Reading
	// getenv reads the keep setting.
	getenv func(string) string
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
		s.targets = func(ctx context.Context, d *Daemon, mode string) (map[string]diskmaint.Target, error) {
			return d.diskMaintenanceTargets(ctx, mode, s.getenv)
		}
	}
	if s.readDisks == nil {
		s.readDisks = noDiskReadings
	}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.dispatch == nil {
		s.dispatch = func(fn func()) { go fn() }
	}
	return s
}

// noDiskReadings is the production disk read until gt-dl4nv.16 adds the df
// readers: no readings means no alert and no tightening, so the pass plans with
// its base keep. It is called on every pass, so the seam the next bead fills is
// already wired.
func noDiskReadings(context.Context) []disks.Reading { return nil }

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

// runDiskMaintenance is the tick: it decides whether this is the window's one
// pass, refuses quietly while the town is busy, and then runs the pass off the
// loop goroutine.
//
// It runs on the loop goroutine and only reads the window marker: the pass
// writes it when it settles a window, and a pass that deferred leaves it alone
// so a later tick in the window tries again.
func (d *Daemon) runDiskMaintenance() {
	if !d.isPatrolActive("disk_maintenance") {
		return
	}
	s := d.diskMaintenanceSeams()
	now := s.now()

	if !shouldRunMaintenanceCycle(now, d.diskMaintenanceSettledAt()) {
		return // this window's pass already ran, refused or failed
	}

	mode, err := diskMaintenanceMode(d.patrolConfig)
	if err != nil {
		// A mode the patrol cannot act on runs nothing, and must not be retried
		// every tick: settle the day and say why once.
		d.settleDiskMaintenanceWindow(now)
		d.logger.Printf("disk_maintenance: not running: %v", err)
		return
	}

	window := diskMaintenanceWindow(d.patrolConfig)
	if !isInMaintenanceWindow(now, window) {
		if _, _, err := maintenanceWindowBounds(now, window); err != nil {
			// A window that does not parse never opens, so the patrol would
			// otherwise be a silent skip forever.
			d.settleDiskMaintenanceWindow(now)
			d.logger.Printf("disk_maintenance: not running: %v", err)
		}
		return
	}

	quiet, why := d.maintenance().quiet(d)
	if !quiet {
		d.logger.Printf("disk_maintenance: skipped: %s", why)
		return
	}
	if !d.diskMaintRunning.CompareAndSwap(false, true) {
		return // a pass is still running
	}
	s.dispatch(func() {
		defer d.diskMaintRunning.Store(false)
		d.diskMaintenancePass(now, mode)
	})
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

// diskMaintenancePass plans every target and, in apply mode, applies the plan
// it just built. A window is settled by anything the pass reaches a conclusion
// about — a kept plan, a refused one, a failed read — and left open when an
// apply deferred, so the next tick in the window retries once the push holding
// the lock is done.
func (d *Daemon) diskMaintenancePass(now time.Time, mode string) {
	ctx, cancel := d.diskMaintenanceContext()
	defer cancel()

	keep, err := d.diskMaintenanceKeep(ctx)
	if err != nil {
		d.logger.Printf("disk_maintenance: not running: %v", err)
		d.settleDiskMaintenanceWindow(now)
		return
	}
	s := d.diskMaintenanceSeams()
	targets, err := s.targets(ctx, d, mode)
	if err != nil {
		d.logger.Printf("disk_maintenance: the targets could not be built: %v", err)
		d.settleDiskMaintenanceWindow(now)
		return
	}
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		d.logger.Printf("disk_maintenance: no maintenance target publishes here; nothing to do")
		d.settleDiskMaintenanceWindow(now)
		return
	}

	apply := mode == agentconfig.DiskMaintenanceModeApply
	policy := diskmaint.Policy{Keep: keep, Apply: apply, Now: now}
	deferred := false
	for _, name := range names {
		if d.diskMaintenanceTarget(ctx, name, targets[name], policy, apply) {
			deferred = true
		}
	}
	if deferred {
		d.logger.Printf("disk_maintenance: apply deferred; the window stays open for another try")
		return
	}
	d.settleDiskMaintenanceWindow(now)
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
// guess (disks.KeepFor).
func (d *Daemon) diskMaintenanceKeep(ctx context.Context) (int, error) {
	s := d.diskMaintenanceSeams()
	base, err := diskMaintKeep(s.getenv)
	if err != nil {
		return 0, err
	}
	keep, err := disks.KeepFor(s.readDisks(ctx), base).Count()
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
func (d *Daemon) diskMaintenanceTargets(ctx context.Context, mode string, getenv func(string) string) (map[string]diskmaint.Target, error) {
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
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot locate the colima docker socket: %w", err)
	}
	ledgerDir := filepath.Join(town, ".runtime", "diskmaint")
	if mode == agentconfig.DiskMaintenanceModeApply {
		if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating the maintenance ledger directory: %w", err)
		}
	}
	gate := &diskMaintPushGate{
		cfg: cfg,
		out: d.diskMaintenanceOut(),
		now: func() time.Time { return d.clk().Now() },
	}
	return map[string]diskmaint.Target{
		"registry": registry.NewTarget(registry.Config{
			Base:         "http://" + cfg.Registry,
			Kubeconfig:   filepath.Join(town, ".runtime", "sbx-k8s", "kubeconfig"),
			RecordsDir:   cfg.SandboxRecords,
			DockerHost:   "unix://" + filepath.Join(home, ".colima", "gt-sbx", "docker.sock"),
			Container:    diskMaintRegistryContainer,
			Keep:         diskMaintDefaultKeep,
			Installed:    diskMaintInstalled(cfg),
			PushInFlight: gate.pushInFlight,
			Lock:         gate.withLock,
			Run:          d.diskMaintRunner,
			Ledger:       diskmaint.NewLedger(filepath.Join(ledgerDir, "ledger.jsonl"), diskMaintLedgerMaxBytes),
		}),
	}, nil
}

// diskMaintenanceOut is where the plugin lock's log lines go.
func (d *Daemon) diskMaintenanceOut() io.Writer {
	if d.logger == nil {
		return io.Discard
	}
	return d.logger.Writer()
}

// diskMaintInstalled reads the installed gt's commit from `gt version`, the
// source the plan names for it. A build that names none is an error, so the
// plan refuses rather than keeping tags for a commit it guessed.
func diskMaintInstalled(cfg polecatimage.Config) func() (string, error) {
	return func() (string, error) {
		commit := (&polecatimage.Run{Cfg: cfg}).InstalledCommit()
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
