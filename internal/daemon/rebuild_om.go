package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/installlock"
	"github.com/steveyegge/gastown/internal/ominstaller"
)

// The rebuild_om job keeps the installed om at the om origin's main, the way
// rebuild_gt does for gt (gt-0f228). Without it the om that reviews landings
// only moves when someone installs by hand, which is the skew om_skew.go
// reports. This job decides; ominstaller.Run installs, in process.
//
// It builds from a clone it owns, never from <town>/om/mayor/rig: an om build
// there races whoever is working in that worktree. The rig is read once, for
// its origin url, and never written.
//
// Opt-in (patrols.rebuild_om.enabled): the install swaps a binary the landing
// worker execs. A cycle that did nothing returns false and retries on the next
// heartbeat; one that reached a verdict holds the interval, as in rebuild_gt.

const (
	defaultRebuildOMInterval = time.Hour

	// rebuildOMStarveAfter is how long a stale om may go uninstalled before the
	// block is escalated. Under it, a landing or a gate suite in flight is the
	// town working as designed.
	rebuildOMStarveAfter = 30 * time.Minute

	// rebuildOMRunBudget bounds one cycle: the clone, the fetch, the lock wait,
	// the drain wait and the build behind them. The drain wait is part of it, so
	// the budget never cuts a cycle inside the wait and leaves the timeout
	// branch unreachable (gt-0hsf6).
	rebuildOMRunBudget = 15*time.Minute + rebuildOMDrainBound

	// defaultRebuildOMDrainAfter is how long a stale om may stay blocked by
	// landing passes before the job holds new ones off: the default for
	// patrols.rebuild_om.drain_after. Under it, a pass in flight is the town
	// working as designed.
	defaultRebuildOMDrainAfter = 10 * time.Minute

	// rebuildOMDrainBound is how long the job waits for the passes in flight
	// once it holds new ones off. It is the bound the upgrade restart gives the
	// same wait (postLandRestartCap), never a longer one (gt-0hsf6).
	rebuildOMDrainBound = postLandRestartCap

	// rebuildOMGitTimeout bounds one git read or write, rebuildOMCloneTimeout
	// the first clone.
	rebuildOMGitTimeout   = 2 * time.Minute
	rebuildOMCloneTimeout = 5 * time.Minute

	// rebuildOMLockWait is how long the clone sync waits for the install lock
	// before deferring: a held lock means an install is building from the clone
	// right now. rebuildOMInstallLockWait is how long the install waits for it.
	rebuildOMLockWait        = 30 * time.Second
	rebuildOMInstallLockWait = 60 * time.Second

	// rebuildOMSrcDir is the job-owned clone, relative to the town.
	rebuildOMSrcDir = ".runtime/om-src"
)

// Alert keys this job owns, one per condition (gt-vwry).
const (
	alertKeyRebuildOMStarved     = "rebuild-om:starved"
	alertKeyRebuildOMInstallFail = "rebuild-om:install-failed"
)

// rebuildOMSource is the source the install's receipt records and the source
// every escalation from this job carries.
const rebuildOMSource = "rebuild-om"

// rebuildOMInstallFunc runs one install and returns its result and exit. The
// job runs ominstaller.Run through it; a test replaces it so no build runs.
type rebuildOMInstallFunc func(ctx context.Context, cfg ominstaller.Config) (ominstaller.Result, ominstaller.Exit)

func rebuildOMInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.RebuildOM != nil {
		if d, err := time.ParseDuration(config.Patrols.RebuildOM.Interval); err == nil && d > 0 {
			return d
		}
	}
	return defaultRebuildOMInterval
}

// triggerRebuildOM runs one cycle on its own goroutine when the job is due: an
// install builds om and can wait on a lock, so inline it would stall the
// heartbeat.
func (d *Daemon) triggerRebuildOM() {
	if !d.isPatrolActive("rebuild_om") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "rebuild_om", d.rebuildOMRun.last(), now, rebuildOMInterval(d.patrolConfig))
	if !dec.due {
		return
	}
	if !d.rebuildOMRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("rebuild_om: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		// Registered first so it runs last, after the guard clears (gt-v0t6k).
		defer d.noteRebuildOMCycleDone()
		defer d.rebuildOMRunning.Store(false)
		if !d.runRebuildOM() {
			return
		}
		d.recordPatrolRun(&d.rebuildOMRun, "rebuild_om", d.clk().Now())
	}()
}

// noteRebuildOMCycleDone is the test seam rebuildOMCycleDoneFn names; it runs
// on the cycle's goroutine, so the hook must not block.
func (d *Daemon) noteRebuildOMCycleDone() {
	if d.rebuildOMCycleDoneFn != nil {
		d.rebuildOMCycleDoneFn()
	}
}

// runRebuildOM is one cycle. It reports whether the cycle reached a verdict
// (om is in force, or the install was refused or failed) as opposed to
// deferring with nothing accomplished.
func (d *Daemon) runRebuildOM() bool {
	ctx, cancel := context.WithTimeout(context.Background(), rebuildOMRunBudget)
	defer cancel()

	cycle := d.startDogCycle("rebuild_om")
	defer cycle.close()
	return d.rebuildOMCycle(ctx, cycle)
}

func (d *Daemon) rebuildOMCycle(ctx context.Context, cycle *dogCycle) bool {
	// Installed side first: an om that cannot describe itself is a local
	// condition, and a fetch for a comparison that cannot happen is wasted.
	omPath := d.omSkewPath()
	installed, err := d.omSkewInstalledSHA(omPath)
	if err != nil {
		cycle.skipStep("installed", "the installed om could not be read: "+err.Error())
		d.logger.Printf("rebuild_om: the installed om at %s could not be read (%v); deferring", omPath, err)
		return false
	}

	src := filepath.Join(d.config.TownRoot, filepath.FromSlash(rebuildOMSrcDir))
	if err := d.rebuildOMEnsureClone(src); err != nil {
		cycle.skipStep("clone", err.Error())
		d.logger.Printf("rebuild_om: %v; deferring", err)
		return false
	}
	head, err := d.rebuildOMFetchHead(src)
	if err != nil {
		cycle.skipStep("fetch", err.Error())
		d.logger.Printf("rebuild_om: %v; deferring", err)
		return false
	}

	if rebuildOMInForce(installed, head) {
		d.rebuildOMReachedForce(cycle, "the installed om is origin/main "+shortOMSHA(head))
		return true
	}

	// Ahead of the busy gate: an om the clone does not show as behind is not
	// waiting on a quiet town, so it must not age toward a starved alert.
	behind, err := d.rebuildOMBehind(src, installed, head)
	if err != nil {
		// Not a verdict: a git that failed to run says nothing about ancestry, so
		// the block and the starved alert stay as they were and the next tick asks again.
		cycle.skipStep("ancestry", "the ancestry check failed: "+err.Error())
		d.logger.Printf("rebuild_om: cannot tell whether the installed om %s is behind origin/main %s (%v); deferring",
			shortOMSHA(installed), shortOMSHA(head), err)
		return false
	}
	if !behind {
		d.rebuildOMBlock.clear()
		d.clearAlerts("the installed om is not behind origin/main", alertKeyRebuildOMStarved)
		cycle.skipStep("ancestry", fmt.Sprintf("the installed om %s is not an ancestor of origin/main %s: ahead of it or unknown to the clone, left in place",
			shortOMSHA(installed), shortOMSHA(head)))
		d.logger.Printf("rebuild_om: the installed om %s is not behind origin/main %s (ahead of it or unknown to the clone); leaving it in place",
			shortOMSHA(installed), shortOMSHA(head))
		return true
	}

	release, why := d.rebuildOMQuietForInstall(ctx, d.clk().Now())
	if release != nil {
		// The hold outlasts the install: a pass starting mid-swap is what it
		// exists to keep off, and every exit from here runs this.
		defer release()
	}
	if why != "" {
		d.rebuildOMNoteBlocked(cycle, "not quiet: "+why)
		return false
	}

	if outcome, why := d.rebuildOMSync(ctx, src); outcome != rebuildGTSyncOK {
		d.rebuildOMNoteBlocked(cycle, why)
		return outcome != rebuildGTSyncDefer
	}

	d.logger.Printf("rebuild_om: installing om %s -> %s in process", shortOMSHA(installed), shortOMSHA(head))
	return d.rebuildOMInstall(ctx, cycle, src, omPath, head)
}

// rebuildOMInForce reports whether the installed om is the origin tip. om
// stamps the full sha, but a short one still counts when it prefixes the tip.
func rebuildOMInForce(installed, head string) bool {
	return strings.HasPrefix(head, installed) || strings.HasPrefix(installed, head)
}

// rebuildOMBehind reports whether the installed om is an ancestor of head in
// the job's clone, the one case where installing origin/main moves om forward.
// An om that is ahead of origin/main (an operator's hotfix not yet landed),
// diverged from it, or a commit the clone has never seen is left alone, as
// rebuild_gt leaves a binary that would be a downgrade. Git's exit 1 and its
// failure to resolve the sha both read as not behind: an install that cannot be
// shown to move forward is not made.
//
// Any other failure (a timeout, a killed git, exit 128 on a broken clone) is
// no answer about ancestry: it comes back as err so the cycle defers instead of
// recording a verdict that holds the interval.
func (d *Daemon) rebuildOMBehind(src, installed, head string) (bool, error) {
	_, err := d.rebuildOMGit(rebuildOMGitTimeout, "-C", src, "merge-base", "--is-ancestor", installed, head)
	if err == nil {
		return true, nil
	}
	var gitErr *rebuildOMGitError
	if errors.As(err, &gitErr) && gitErr.answersNotAncestor() {
		return false, nil
	}
	return false, err
}

// rebuildOMEnsureClone makes sure the job's clone exists, cloning it from the
// om rig's origin url on first use. The rig is only asked for that url.
func (d *Daemon) rebuildOMEnsureClone(src string) error {
	_, err := os.Stat(filepath.Join(src, ".git"))
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot read the om clone at %s: %w", src, err)
	}
	rig := filepath.Join(d.config.TownRoot, filepath.FromSlash(omSkewRigWorktree))
	url, err := d.rebuildOMGit(rebuildOMGitTimeout, "-C", rig, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("cannot read the om origin url: %w", err)
	}
	url = strings.TrimSpace(url)
	if url == "" {
		return fmt.Errorf("the om rig at %s has no origin url", rig)
	}
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(src), err)
	}
	if _, err := d.rebuildOMGit(rebuildOMCloneTimeout, "clone", "--quiet", "--", url, src); err != nil {
		return fmt.Errorf("cannot clone the om origin into %s: %w", src, err)
	}
	d.logger.Printf("rebuild_om: cloned the om origin into %s", src)
	return nil
}

// rebuildOMFetchHead fetches the clone and returns origin/main's tip. Only the
// remote-tracking ref moves: the checked-out tree stays what a running install
// is building until rebuildOMSync advances it under the lock.
func (d *Daemon) rebuildOMFetchHead(src string) (string, error) {
	if _, err := d.rebuildOMGit(rebuildOMGitTimeout, "-C", src, "fetch", "origin", "--quiet"); err != nil {
		return "", fmt.Errorf("cannot fetch the om origin: %w", err)
	}
	out, err := d.rebuildOMGit(rebuildOMGitTimeout, "-C", src, "rev-parse", "origin/main")
	if err != nil {
		return "", fmt.Errorf("cannot read origin/main in the om clone: %w", err)
	}
	head := strings.TrimSpace(out)
	if !isCommitSHA(head) {
		return "", fmt.Errorf("origin/main in the om clone read as %q, not a commit", head)
	}
	return head, nil
}

// rebuildOMBusy names what keeps the town from an install right now, or "" when
// it is quiet. A landing in flight is reviewed by the om being replaced, and a
// gate suite or container competes with the build for CPU.
func (d *Daemon) rebuildOMBusy() string {
	if n := d.landingPasses.Load(); n > 0 {
		return fmt.Sprintf("%d landing pass(es) in flight", n)
	}
	busy, _ := d.rebuildGTGate()
	return busy
}

// rebuildOMQuietForInstall reports whether the cycle may install now: "" when
// the town is quiet, else what still holds it. A busy town is left to the next
// heartbeat unless the block has outlasted the drain wait and a landing pass is
// the blocker; then the job takes the landing hold the upgrade restart uses,
// waits for the passes in flight up to rebuildOMDrainBound, and returns the
// release for the cycle to defer across the install (gt-0hsf6).
func (d *Daemon) rebuildOMQuietForInstall(ctx context.Context, now time.Time) (release func(), why string) {
	busy := d.rebuildOMBusy()
	if busy == "" {
		return nil, ""
	}
	age, open := d.rebuildOMBlock.openFor(now)
	if !open || age < d.rebuildOMDrainAfter() {
		return nil, busy
	}
	// Only a landing pass is what the drain frees: a gate suite ends on its own
	// and is not this job's to stop.
	if d.landingPasses.Load() == 0 {
		return nil, busy
	}

	idle := d.landingHolds.hold(landingHoldRebuildOM)
	release = func() { d.landingHolds.release(landingHoldRebuildOM) }
	d.logger.Printf("rebuild_om: stale and blocked for %s behind %s; holding new landing passes off for up to %s",
		age.Round(time.Minute), busy, rebuildOMDrainBound)
	d.noteRebuildOMDrain()
	// The count is read only after the hold is on, so a pass that ended in
	// between is seen here rather than waited out to the bound.
	if d.landingPasses.Load() == 0 {
		d.logger.Printf("rebuild_om: the landing passes in flight have finished; installing")
		return release, ""
	}
	select {
	case <-idle:
		d.logger.Printf("rebuild_om: the landing passes in flight have finished; installing")
	case <-d.clk().After(rebuildOMDrainBound):
		// Installing beside a pass is what the busy gate refuses: the pass is
		// reviewed by the om being replaced. The hold still stops the queue
		// growing, so the next heartbeat finds the town quiet.
		return release, fmt.Sprintf("landing passes still in flight after %s", rebuildOMDrainBound)
	case <-ctx.Done():
		return release, "the drain wait was cut off by the cycle budget"
	}
	if busy := d.rebuildOMBusy(); busy != "" {
		return release, busy
	}
	return release, ""
}

// noteRebuildOMDrain tells a test the hold is on (see rebuildOMDrainFn).
func (d *Daemon) noteRebuildOMDrain() {
	if d.rebuildOMDrainFn != nil {
		d.rebuildOMDrainFn()
	}
}

// rebuildOMDrainAfter is how long a blocked om runs before the job holds new
// landing passes off: patrols.rebuild_om.drain_after, default 10m. A value that
// is not a positive duration takes the default and says so, because a block
// nobody drains is the starvation this wait exists to end (gt-0hsf6).
func (d *Daemon) rebuildOMDrainAfter() time.Duration {
	cfg := d.patrolConfig
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.RebuildOM == nil {
		return defaultRebuildOMDrainAfter
	}
	raw := cfg.Patrols.RebuildOM.DrainAfter
	if raw == "" {
		return defaultRebuildOMDrainAfter
	}
	wait, err := time.ParseDuration(raw)
	if err != nil || wait <= 0 {
		d.logger.Printf("rebuild_om: patrols.rebuild_om.drain_after %q is not a positive duration; using %s",
			raw, defaultRebuildOMDrainAfter)
		return defaultRebuildOMDrainAfter
	}
	return wait
}

// rebuildOMSync fast-forwards the clone's checkout to origin/main under the
// install lock, so the tree cannot move under a build already running from it.
func (d *Daemon) rebuildOMSync(ctx context.Context, src string) (rebuildGTSyncOutcome, string) {
	lockPath := filepath.Join(d.rebuildOMDaemonDir(), "install-om.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return rebuildGTSyncDefer, "cannot create the install lock directory: " + err.Error()
	}
	lockCtx, cancel := context.WithTimeout(ctx, rebuildOMLockWait)
	defer cancel()
	lock := flock.New(lockPath)
	locked, err := lock.TryLockContext(lockCtx, 250*time.Millisecond)
	if err != nil || !locked {
		return rebuildGTSyncDefer, "another install holds the install lock past " + rebuildOMLockWait.String()
	}
	defer func() { _ = lock.Unlock() }()

	if _, err := d.rebuildOMGit(rebuildOMGitTimeout, "-C", src, "merge", "--ff-only", "origin/main", "--quiet"); err != nil {
		// The clone is the job's own, so a tree that will not fast-forward is a
		// state no retry clears.
		return rebuildGTSyncRefuse, "the om clone cannot fast-forward to origin/main: " + err.Error()
	}
	return rebuildGTSyncOK, ""
}

// rebuildOMInstall runs the install in process and maps its exit and result,
// the contract ominstaller.Run documents: 0 installed or already in force, 1
// failed (the previous om stays), 2 refused, 3 busy.
func (d *Daemon) rebuildOMInstall(ctx context.Context, cycle *dogCycle, src, omPath, head string) bool {
	res, exit := d.runOMInstall(ctx, d.rebuildOMInstallConfig(src, omPath))
	d.logger.Printf("rebuild_om: install-om finished (%s)", reasonOr(res.Event, "no result"))

	// A run the cycle's budget cut off holds no verdict: the install may have
	// stopped partway, and the next heartbeat asks again.
	if ctx.Err() != nil {
		d.rebuildOMNoteBlocked(cycle, "install-om was cut off by the cycle budget before it reported a result")
		return false
	}

	switch exit {
	case ominstaller.ExitInstalled:
		cycle.closeStep("install")
		d.rebuildOMReachedForce(cycle, fmt.Sprintf("install-om %s: om is %s", reasonOr(res.Event, "ok"), shortOMSHA(reasonOr(res.Commit, head))))
		return true
	case ominstaller.ExitRefused:
		cycle.skipStep("install", "install-om refused: "+reasonOr(res.Reason, "unknown"))
		d.rebuildOMNoteBlocked(cycle, "install-om refused: "+reasonOr(res.Reason, "unknown"))
		return true
	case ominstaller.ExitBusy:
		d.rebuildOMNoteBlocked(cycle, "install-om deferred: "+reasonOr(res.Reason, "unknown"))
		return false
	default:
		// The install left the previous om in force: it never swaps without a way
		// back, and a failed smoke check renames the old binary back.
		reason := reasonOr(res.Reason, fmt.Sprintf("exit %d", exit))
		cycle.failStep("install", "install-om failed: "+reason)
		_ = d.rebuildOMAlert(alertKeyRebuildOMInstallFail,
			fmt.Sprintf("rebuild_om: install-om failed (%s) installing %s; the previous om stays in force", reason, shortOMSHA(head)))
		return true
	}
}

// rebuildOMInstallConfig is the install the cycle asks for: the job's clone,
// the om the landing worker resolves, the job's lock wait and receipt source.
// Signing, timeouts and any INSTALL_OM_* override resolve from the environment
// in runOMInstall.
func (d *Daemon) rebuildOMInstallConfig(src, omPath string) ominstaller.Config {
	return ominstaller.Config{
		OMPath:    omPath,
		TownRoot:  d.config.TownRoot,
		SrcDir:    src,
		DaemonDir: d.rebuildOMDaemonDir(),
		Source:    rebuildOMSource,
		LockWait:  rebuildOMInstallLockWait,
		Exec:      d.omInstallExec,
		Acquire:   installlock.Acquire,
		Logf:      d.logger.Printf,
	}
}

// runOMInstall runs one install in process. The config is resolved from the
// environment first, so an operator's INSTALL_OM_* settings apply. A config
// that does not resolve fails the install without running it.
func (d *Daemon) runOMInstall(ctx context.Context, cfg ominstaller.Config) (ominstaller.Result, ominstaller.Exit) {
	if d.rebuildOMInstallFn != nil {
		return d.rebuildOMInstallFn(ctx, cfg)
	}
	resolved, err := ominstaller.Resolve(cfg)
	if err != nil {
		d.logger.Printf("rebuild_om: install-om settings do not resolve, so nothing was installed: %v", err)
		return ominstaller.Result{Event: "failed", Reason: "unexpected"}, ominstaller.ExitFailed
	}
	return ominstaller.Run(ctx, resolved)
}

// omInstallExec is the command runner the install's commands go through: the
// daemon's command seam, in the directory the install names.
func (d *Daemon) omInstallExec(ctx context.Context, c ominstaller.Command) (string, string, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	stdout, stderr, err := d.runCmd(cmd)
	return string(stdout), string(stderr), err
}

// rebuildOMDaemonDir is the directory the install lock and the receipts file
// sit in: INSTALL_OM_DAEMON_DIR, else <town>/daemon.
func (d *Daemon) rebuildOMDaemonDir() string {
	if dir := os.Getenv("INSTALL_OM_DAEMON_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(d.config.TownRoot, "daemon")
}

// rebuildOMAlert raises a MEDIUM alert under key. The delivery outcome is
// returned for the caller that retries on a drop.
func (d *Daemon) rebuildOMAlert(key, message string) error {
	return d.escalateAlertSeverity("MEDIUM", key, rebuildOMSource, message)
}

// rebuildOMNoteBlocked records a cycle that left a stale om uninstalled: it
// opens (or ages) the block and escalates once, when the block outlasts
// rebuildOMStarveAfter. One firing per episode keeps a long block from costing
// a Dolt commit per heartbeat (gt-kox0).
func (d *Daemon) rebuildOMNoteBlocked(cycle *dogCycle, reason string) {
	cycle.skipStep("blocked", reason)
	age := d.rebuildOMBlock.open(d.clk().Now())
	d.logger.Printf("rebuild_om: stale and not installed for %s: %s", age.Round(time.Minute), reason)
	if age < rebuildOMStarveAfter || !d.rebuildOMBlock.alertOnce() {
		return
	}
	if err := d.rebuildOMAlert(alertKeyRebuildOMStarved,
		fmt.Sprintf("rebuild-om: the om has been stale and blocked from install for %s; last: %s",
			age.Round(time.Minute), reason)); err != nil {
		// The escalation never reached the town: let the next cycle retry
		// rather than record a firing nobody saw.
		d.logger.Printf("rebuild_om: starvation escalation did not reach the town; retrying next cycle: %v", err)
		d.rebuildOMBlock.mu.Lock()
		d.rebuildOMBlock.alerted = false
		d.rebuildOMBlock.mu.Unlock()
	}
}

// rebuildOMReachedForce records that om is current and closes every alert this
// job owns: each asserts om is out of force, and it no longer is.
func (d *Daemon) rebuildOMReachedForce(cycle *dogCycle, reason string) {
	d.rebuildOMBlock.clear()
	cycle.closeStep("force")
	d.logger.Printf("rebuild_om: %s", reason)
	d.clearAlerts("the installed om is in force", alertKeyRebuildOMStarved, alertKeyRebuildOMInstallFail)
}

// rebuildOMGitError is a failed git command. It reads as git's own message (or
// the exec error when git printed none) and keeps the exit status, which is -1
// when git never exited on its own: a timeout, a signal, a failure to start.
type rebuildOMGitError struct {
	msg  string
	code int
}

func (e *rebuildOMGitError) Error() string { return e.msg }

// answersNotAncestor reports whether a failed `merge-base --is-ancestor` is
// git's answer rather than a failure to ask: exit 1 says "not an ancestor",
// and exit 128 with an unresolvable sha says the clone has never seen the
// commit. Any other 128 (not a repository, a corrupt object store) is not.
func (e *rebuildOMGitError) answersNotAncestor() bool {
	switch e.code {
	case 1:
		return true
	case 128:
		msg := strings.ToLower(e.msg)
		return strings.Contains(msg, "not a valid commit name") || strings.Contains(msg, "not a valid object name")
	}
	return false
}

// rebuildOMGit runs one git command, returning stdout. It never prompts: the
// daemon has no terminal to answer a credential request. A failure is a
// *rebuildOMGitError.
func (d *Daemon) rebuildOMGit(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	stdout, stderr, err := d.runCmd(cmd)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = err.Error()
		}
		code := -1
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return string(stdout), &rebuildOMGitError{msg: msg, code: code}
	}
	return string(stdout), nil
}
