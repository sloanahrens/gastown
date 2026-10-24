package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/version"
)

// The rebuild_om job keeps the installed om at the om origin's main, the way
// rebuild_gt does for gt (gt-0f228). Without it the om that reviews landings
// only moves when someone installs by hand, which is the skew om_skew.go
// reports. This job decides; scripts/install-om.sh installs.
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

	// rebuildOMRunBudget bounds one cycle: the clone, the fetch, the lock wait
	// and the build behind them.
	rebuildOMRunBudget = 15 * time.Minute

	// rebuildOMGitTimeout bounds one git read or write, rebuildOMCloneTimeout
	// the first clone.
	rebuildOMGitTimeout   = 2 * time.Minute
	rebuildOMCloneTimeout = 5 * time.Minute

	// rebuildOMLockWait is how long the clone sync waits for install-om.sh's
	// lock before deferring: a held lock means an install is building from the
	// clone right now. rebuildOMInstallLockWait is what the script waits for it.
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

// rebuildOMSource is the --source install-om.sh records and the source every
// escalation from this job carries.
const rebuildOMSource = "rebuild-om"

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

	if busy := d.rebuildOMBusy(); busy != "" {
		d.rebuildOMNoteBlocked(cycle, "not quiet: "+busy)
		return false
	}

	if outcome, why := d.rebuildOMSync(ctx, src); outcome != rebuildGTSyncOK {
		d.rebuildOMNoteBlocked(cycle, why)
		return outcome != rebuildGTSyncDefer
	}

	d.logger.Printf("rebuild_om: installing om %s -> %s through scripts/install-om.sh", shortOMSHA(installed), shortOMSHA(head))
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

// rebuildOMSync fast-forwards the clone's checkout to origin/main under
// install-om.sh's lock, so the tree cannot move under a build already running
// from it.
func (d *Daemon) rebuildOMSync(ctx context.Context, src string) (rebuildGTSyncOutcome, string) {
	lockPath := filepath.Join(d.config.TownRoot, "daemon", "install-om.lock")
	if dir := os.Getenv("INSTALL_OM_DAEMON_DIR"); dir != "" {
		lockPath = filepath.Join(dir, "install-om.lock")
	}
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

// rebuildOMInstall runs scripts/install-om.sh and maps its exit code and
// RESULT line, the contract install-om.sh documents.
func (d *Daemon) rebuildOMInstall(ctx context.Context, cycle *dogCycle, src, omPath, head string) bool {
	repoRoot, err := version.GetRepoRootForTown(d.config.TownRoot)
	if err != nil {
		cycle.failStep("installer", "no gt source checkout holds scripts/install-om.sh")
		_ = d.rebuildOMAlert(alertKeyRebuildOMInstallFail,
			"rebuild_om: no gt source checkout under the town, so nothing can run scripts/install-om.sh")
		return true
	}
	installer := filepath.Join(repoRoot, "scripts", "install-om.sh")
	if _, err := os.Stat(installer); err != nil {
		cycle.failStep("installer", "scripts/install-om.sh is missing")
		_ = d.rebuildOMAlert(alertKeyRebuildOMInstallFail,
			fmt.Sprintf("rebuild_om: %s is missing, so nothing can install om", installer))
		return true
	}

	out := &rebuildGTInstallLog{}
	cmd := exec.CommandContext(ctx, "bash", installer, "--om-path", omPath, "--source", rebuildOMSource)
	cmd.Dir = repoRoot
	// The script builds the clone this job just synced and installs the om the
	// landing worker resolves; the environment pins both so an operator's
	// INSTALL_OM_* cannot point it elsewhere.
	cmd.Env = append(os.Environ(),
		"GT_TOWN_ROOT="+d.config.TownRoot,
		"INSTALL_OM_SRC_DIR="+src,
		"INSTALL_OM_LOCK_WAIT="+strconv.Itoa(int(rebuildOMInstallLockWait.Seconds())),
	)
	cmd.Stdout = out
	cmd.Stderr = out

	_, _, runErr := d.runCmd(cmd)
	res := parseOMInstallResult(out.text())
	d.logger.Printf("rebuild_om: install-om finished (%s); last lines:\n%s", reasonOr(res.event, "no result"), out.tail(rebuildGTLogTailLines))

	code := 0
	if runErr != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	// A run the cycle's budget or a signal killed never printed its RESULT
	// line, so there is no verdict to hold the interval over.
	if ctx.Err() != nil || code < 0 {
		d.rebuildOMNoteBlocked(cycle, "install-om was killed before it reported a result")
		return false
	}

	switch code {
	case 0:
		cycle.closeStep("install")
		d.rebuildOMReachedForce(cycle, fmt.Sprintf("install-om %s: om is %s", reasonOr(res.event, "ok"), shortOMSHA(reasonOr(res.commit, head))))
		return true
	case 2:
		cycle.skipStep("install", "install-om refused: "+reasonOr(res.reason, "unknown"))
		d.rebuildOMNoteBlocked(cycle, "install-om refused: "+reasonOr(res.reason, "unknown"))
		return true
	case 3:
		d.rebuildOMNoteBlocked(cycle, "install-om deferred: "+reasonOr(res.reason, "unknown"))
		return false
	default:
		// install-om has no escalation of its own, and it left the previous om in
		// force: it never swaps without a way back, and a failed smoke check
		// renames the old binary back.
		reason := reasonOr(res.reason, fmt.Sprintf("exit %d", code))
		cycle.failStep("install", "install-om failed: "+reason)
		_ = d.rebuildOMAlert(alertKeyRebuildOMInstallFail,
			fmt.Sprintf("rebuild_om: install-om failed (%s) installing %s; the previous om stays in force", reason, shortOMSHA(head)))
		return true
	}
}

// text returns everything the installer printed.
func (w *rebuildGTInstallLog) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// parseOMInstallResult reads install-om.sh's one machine-readable line,
// `install-om: RESULT <event> <commit> <prev> <reason>`, where a field it has
// nothing to say about is "-". The last such line wins.
func parseOMInstallResult(text string) rebuildGTInstallResult {
	var res rebuildGTInstallResult
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || fields[0] != "install-om:" || fields[1] != "RESULT" {
			continue
		}
		fields = append(fields, "-", "-", "-", "-")
		res = rebuildGTInstallResult{
			event:  fields[2],
			commit: dashToEmpty(fields[3]),
			prev:   dashToEmpty(fields[4]),
			reason: dashToEmpty(fields[5]),
		}
	}
	return res
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
