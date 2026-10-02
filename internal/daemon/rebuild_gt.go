package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/version"
)

// The rebuild_gt job brings the installed gt binary in force from main (was
// the rebuild-gt run.sh plugin, gt-4k3fj.8.6).
//
// The daemon's own upgrade path does not cover this: checkUpgradeRestart only
// restarts the daemon onto a binary something else already installed, so
// without this job a merged commit stays inert until an operator runs `make
// install` (gt-oqbw). This job decides; scripts/install-gt.sh installs.
//
// Three things the plugin carried are not here. Its gt plugin record-run
// receipts: a daemon job records a dog_cycle line instead (gt-4k3fj.8.1). Its
// exit-code contract with the plugin dispatcher: a cycle that accomplished
// nothing returns false and the next heartbeat retries, one that reached a
// verdict holds the interval. Its daemon-not-in-force backstop:
// checkUpgradeRestart already escalates a restart marker that waits as
// daemon:restart-pending-stuck at the same 30m.

const (
	// defaultRebuildGTInterval is how long a cycle that reached a verdict
	// holds this job off. A cycle that accomplished nothing retries on the
	// next heartbeat: an hourly-only tick kept landing inside busy windows and
	// starved the install (gt-kox0).
	defaultRebuildGTInterval = time.Hour

	// rebuildGTInstallThreshold is how many commits behind the binary must be
	// to be due. One merged commit not in force is a live defect, not a
	// rounding error: this job is the backstop for landings nobody installed.
	rebuildGTInstallThreshold = 1

	// rebuildGTMaxCommitsBehind is where a due binary stops being merely due
	// and becomes drift worth an operator's attention.
	rebuildGTMaxCommitsBehind = 20

	// rebuildGTStarveAfter is how long a due binary may go uninstalled before
	// the block is escalated and, when the container-gate slot is the blocker,
	// waited for instead of raced. Under it, a busy gate is the town working
	// as designed (gate holds measured 2m-20m20s on 2026-09-22); past it, the
	// block is the thing to report.
	rebuildGTStarveAfter = 30 * time.Minute

	// rebuildGTReserveWait bounds the wait for a gate to release its slot,
	// together with the acquire that follows it.
	rebuildGTReserveWait = 10 * time.Minute

	// rebuildGTMinReserve is the floor on the slot wait handed to install-gt.sh
	// once the reserve budget is spent; a zero would disable its own wait.
	rebuildGTMinReserve = time.Minute

	// rebuildGTGatePoll is how often the reserve wait re-reads the slot.
	rebuildGTGatePoll = 15 * time.Second

	// rebuildGTLockWait is how long the sync waits for install-gt.sh's lock
	// before deferring to the next heartbeat: a lock still held means an
	// install is running right now.
	rebuildGTLockWait = 30 * time.Second

	// rebuildGTInstallLockWait is what install-gt.sh waits for the same lock.
	// A minute, not its own five: a longer wait only keeps this job running,
	// which keeps the daemon non-idle and delays its upgrade restart.
	rebuildGTInstallLockWait = 60 * time.Second

	// rebuildGTRunBudget bounds one cycle: the reserve wait, both lock waits,
	// and the build and install behind them.
	rebuildGTRunBudget = 25 * time.Minute

	// rebuildGTGitTimeout bounds one git call.
	rebuildGTGitTimeout = 2 * time.Minute

	// rebuildGTSubjectLimit caps the commit subjects the install records.
	rebuildGTSubjectLimit = 20

	// rebuildGTLogTailLines is how much of install-gt.sh's output the daemon
	// log keeps. The whole build log is unbounded and unread; what a failure is
	// read back from is its end.
	rebuildGTLogTailLines = 20
)

// Alert keys this job owns. Each is the stable identity of one condition, so
// a condition that persists leaves one open escalation rather than one per
// cycle (gt-vwry).
const (
	alertKeyRebuildGTDrift        = "rebuild-gt:drift"
	alertKeyRebuildGTDriftUnknown = "rebuild-gt:drift-unknown"
	alertKeyRebuildGTStarved      = "rebuild-gt:starved"
	alertKeyRebuildGTInstallFail  = "rebuild-gt:install-failed"
	alertKeyRebuildGTNoInstaller  = "rebuild-gt:no-installer"

	// alertKeyRebuildGTUnverified is retired (nothing raises it) and kept in
	// the clear list because an escalation opened before it was retired has no
	// other closer.
	alertKeyRebuildGTUnverified = "rebuild-gt:unverified"
)

// rebuildGTSource is the --source every escalation and install from this job
// carries, the same string the plugin used.
const rebuildGTSource = "rebuild-gt"

// rebuildGTSlotRole is the role the install takes a container-gate slot under
// when it has waited one out.
const rebuildGTSlotRole = "gastown/rebuild-gt"

// rebuildGTBlock is the job's one piece of memory: when the current "due and
// not installed" episode opened, and whether its escalation has gone out.
// In-memory on purpose — the daemon is the process that restarts to consume
// an install, so an episode that outlives it is over (the plugin persisted
// the same clock to daemon/rebuild-gt-state.json, gt-kox0).
type rebuildGTBlock struct {
	mu      sync.Mutex
	since   time.Time
	alerted bool
}

// open records one cycle that left a due binary out of force and returns how
// long the episode has been running, counting this cycle.
func (b *rebuildGTBlock) open(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.since.IsZero() {
		b.since = now
	}
	return now.Sub(b.since)
}

// clear closes the episode: the binary is in force, or not due at all.
func (b *rebuildGTBlock) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.since = time.Time{}
	b.alerted = false
}

// alertOnce reports whether this episode's escalation has not been raised
// yet, and marks it raised. One firing per episode: the escalation is what
// makes the block visible, and a firing per heartbeat would be Dolt commits
// that buy nothing (gt-kox0, gt-vwry).
func (b *rebuildGTBlock) alertOnce() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.alerted {
		return false
	}
	b.alerted = true
	return true
}

// open reports whether an episode is running, without opening one.
func (b *rebuildGTBlock) openFor(now time.Time) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.since.IsZero() {
		return 0, false
	}
	return now.Sub(b.since), true
}

func rebuildGTInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.RebuildGT != nil {
		if d, err := time.ParseDuration(config.Patrols.RebuildGT.Interval); err == nil && d > 0 {
			return d
		}
	}
	return defaultRebuildGTInterval
}

// triggerRebuildGT runs one rebuild_gt cycle on its own goroutine when the
// job is due: an install builds the whole binary and can wait minutes for a
// container-gate slot, so inline it would stall the heartbeat.
func (d *Daemon) triggerRebuildGT() {
	if !d.isPatrolActive("rebuild_gt") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "rebuild_gt", time.Time{}, now, rebuildGTInterval(d.patrolConfig))
	// A request armed by a landing pass makes the job due on its own: an
	// install is the point of the drain signal, and the interval it would
	// otherwise wait out is what leaves the merge inert (gt-3qmv4.1).
	if !dec.due && !d.rebuildGTRequested.Load() {
		return
	}
	if !d.rebuildGTRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.due && dec.warn != "" {
		d.logger.Printf("rebuild_gt: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.rebuildGTRunning.Store(false)
		if !d.runRebuildGT() {
			// Nothing accomplished: leave the gate open so the next
			// heartbeat retries (the script's exit 3), and leave a request
			// armed so a not-quiet deferral does not lose it.
			return
		}
		d.rebuildGTRequested.Store(false)
		if err := savePatrolLastRun(d.config.TownRoot, "rebuild_gt", d.clk().Now()); err != nil {
			d.logger.Printf("rebuild_gt: WARNING: cannot persist last-run time (%v)", err)
		}
	}()
}

// requestRebuildGTInstall arms the sticky request that makes the next
// rebuild_gt heartbeat due regardless of the patrol's interval. A cycle clears
// it only on a verdict, so a deferral keeps the install pending; arming an
// already-armed request is a no-op, which is what keeps the log one line per
// drain rather than one per landing pass (gt-3qmv4.1).
func (d *Daemon) requestRebuildGTInstall() {
	if d.rebuildGTRequested.CompareAndSwap(false, true) {
		d.logger.Printf("rebuild_gt: install requested: queue drained after landing")
	}
}

// requestRebuildGTInstallNow arms the same request for a bead the operator
// labeled gt:install-now: its landing asks for the install at once, so the
// request is not held back for the queue to drain (gt-3qmv4.2).
func (d *Daemon) requestRebuildGTInstallNow() {
	if d.rebuildGTRequested.CompareAndSwap(false, true) {
		d.logger.Printf("rebuild_gt: install requested: a %s bead landed", landworker.LabelInstallNow)
	}
}

// runRebuildGT is one cycle. It reports whether the cycle reached a verdict
// (the binary is fresh, in force, or safely refused) as opposed to deferring
// with nothing accomplished.
func (d *Daemon) runRebuildGT() bool {
	ctx, cancel := context.WithTimeout(context.Background(), rebuildGTRunBudget)
	defer cancel()

	cycle := d.startDogCycle("rebuild_gt")
	defer cycle.close()

	now := d.clk().Now()
	repoRoot, err := version.GetRepoRootForTown(d.config.TownRoot)
	if err != nil {
		// Nothing to rebuild and nothing to watch: a checkout that is not
		// there cannot be stale.
		cycle.skipStep("repo", "no gt source checkout under the town")
		d.logger.Printf("rebuild_gt: no gt source checkout under %s; nothing to rebuild", d.config.TownRoot)
		return true
	}

	return d.rebuildGTCycle(ctx, cycle, repoRoot, now)
}

// rebuildGTCycle is the script's flow, in its order.
func (d *Daemon) rebuildGTCycle(ctx context.Context, cycle *dogCycle, repoRoot string, now time.Time) bool {
	// The stagnation check runs before any bail, and reads git history only:
	// every skip below leaves a due binary out of force, and keying the alarm
	// to that outcome rather than to an enumeration of skip reasons is what
	// catches a bail path nobody has written yet (gt-bce, gt-kox0).
	before, ok := d.rebuildGTStale(repoRoot)
	if !ok {
		// An unreadable staleness check accomplished nothing, and it has shown
		// nothing about the binary, so it neither opens nor closes a block.
		cycle.skipStep("stale", "staleness could not be read")
		d.logger.Printf("rebuild_gt: staleness could not be read; deferring")
		return false
	}
	d.rebuildGTDrift(before)

	due := before.IsStale && (before.CommitsBehind == 0 || before.CommitsBehind >= rebuildGTInstallThreshold)

	// A not-due reading closes the starvation clock, and only that clock: it
	// compares the binary against the checkout's own main ref, which may
	// itself be behind origin, so a "fresh" verdict here is not yet evidence
	// about the binary. The sync below is what makes the second read decisive
	// (gt-h8s8, gt-kox0).
	if !due {
		d.rebuildGTNotDue(cycle, "the binary is fresh against the local main ref")
	}

	// Pre-flight: only tracked changes outside .beads/ can change what
	// `make build` produces, and a checkout on another branch is not main's
	// to build (gt-50k).
	if dirty, err := d.rebuildGTGit(repoRoot, "status", "--porcelain", "--untracked-files=no", "--", ".", ":(exclude).beads"); err != nil {
		cycle.skipStep("preflight", "git status failed: "+err.Error())
		d.logger.Printf("rebuild_gt: git status failed (%v); deferring", err)
		return false
	} else if strings.TrimSpace(dirty) != "" {
		cycle.skipStep("preflight", "repo has uncommitted changes")
		d.rebuildGTNoteBlocked(cycle, due, "repo has uncommitted changes")
		return true
	}

	branch, err := d.rebuildGTGit(repoRoot, "branch", "--show-current")
	if err != nil {
		cycle.skipStep("preflight", "git branch failed: "+err.Error())
		d.logger.Printf("rebuild_gt: git branch failed (%v); deferring", err)
		return false
	}
	if strings.TrimSpace(branch) != "main" {
		cycle.skipStep("preflight", "not on main branch (on "+strings.TrimSpace(branch)+")")
		d.rebuildGTNoteBlocked(cycle, due, "not on main branch")
		return true
	}

	if outcome, why := d.rebuildGTSync(ctx, repoRoot); outcome != rebuildGTSyncOK {
		if outcome == rebuildGTSyncDefer {
			d.rebuildGTNoteBlocked(cycle, due, why)
			return false
		}
		cycle.skipStep("sync", why)
		d.rebuildGTNoteBlocked(cycle, due, why)
		return true
	}

	// Read staleness again, after the sync: it compares the binary against the
	// checkout's own main ref, so checking first would let a stale checkout
	// with a binary built from that same stale tip read as fresh (gt-h8s8).
	after, ok := d.rebuildGTStale(repoRoot)
	if !ok {
		cycle.skipStep("stale", "staleness could not be read after the sync")
		d.logger.Printf("rebuild_gt: staleness could not be read after the sync; deferring")
		return false
	}

	if !after.IsStale {
		d.rebuildGTReachedForce(cycle, "the binary is fresh")
		return true
	}
	// SafeToRebuild is stale AND forward-only AND on a build branch: a rebuild
	// that would be a downgrade must never happen.
	if !after.IsForward || !after.OnMainBranch {
		cycle.skipStep("detect", "not safe to rebuild (not on main, or a downgrade)")
		d.rebuildGTNoteBlocked(cycle, true, "not safe to rebuild")
		return true
	}

	// An unmeasurable count is due, not under threshold: reading it as 0 is
	// what let a stale, quiet, safe-to-rebuild binary sit deferred forever
	// (gt-oqbw). The check reports 0 both for "nothing to compare" and for a
	// count it could not parse, and a stale binary is never legitimately 0
	// behind, so 0 here means unknown.
	behind := "unknown"
	if after.CommitsBehind > 0 {
		behind = strconv.Itoa(after.CommitsBehind)
	}
	if after.CommitsBehind > 0 && after.CommitsBehind < rebuildGTInstallThreshold {
		d.rebuildGTNotDue(cycle, fmt.Sprintf("binary is %d behind (under the install threshold %d)", after.CommitsBehind, rebuildGTInstallThreshold))
		return true
	}

	// Yield to a running gate: `make build` competes for CPU with a suite
	// whose tests are load-sensitive, and the install itself takes a slot when
	// it has waited one out (gt-htx3, gt-kox0).
	//
	// reserveTimeout is seconds the install may wait for the slot once it runs.
	// Zero means it does not take one at all.
	reserveTimeout := 0
	if busy, reservable := d.rebuildGTGate(); busy != "" {
		age, _ := d.rebuildGTBlock.openFor(now)
		switch {
		case reservable && age >= rebuildGTStarveAfter:
			d.rebuildGTNoteBlocked(cycle, true, "not quiet: "+busy)
			d.logger.Printf("rebuild_gt: blocked %s: waiting up to %s for the gate to release the slot",
				age.Round(time.Minute), rebuildGTReserveWait)
			deadline := now.Add(rebuildGTReserveWait)
			if !d.rebuildGTWaitForGate(ctx, deadline) {
				d.logger.Printf("rebuild_gt: waited %s for the container-gate slot and did not get it; deferring", rebuildGTReserveWait)
				return false
			}
			// The wait and the acquire behind it share one budget, so what is
			// left of it is what the install may wait — with a floor, so a gate
			// that takes the slot the moment the wait ends still gets one
			// honest acquire.
			reserveTimeout = int(deadline.Sub(d.clk().Now()).Seconds())
			if reserveTimeout < int(rebuildGTMinReserve.Seconds()) {
				reserveTimeout = int(rebuildGTMinReserve.Seconds())
			}
		default:
			d.rebuildGTNoteBlocked(cycle, true, "not quiet: "+busy)
			return false
		}
	}

	d.logger.Printf("rebuild_gt: installing %s (%s commits behind) through scripts/install-gt.sh", repoRoot, behind)
	return d.rebuildGTInstall(ctx, cycle, repoRoot, reserveTimeout)
}

// rebuildGTSyncOutcome is how the pre-install sync of the source checkout ended.
type rebuildGTSyncOutcome int

const (
	rebuildGTSyncOK rebuildGTSyncOutcome = iota
	// rebuildGTSyncDefer: nothing accomplished, retry next heartbeat.
	rebuildGTSyncDefer
	// rebuildGTSyncRefuse: a state a retry cannot fix on its own.
	rebuildGTSyncRefuse
)

// rebuildGTSync fast-forwards the source checkout to origin/main under
// install-gt.sh's lock. Every write to that tree happens under this lock, so
// the sync cannot move it under a build a `make install` is running
// (claude-7fc).
func (d *Daemon) rebuildGTSync(ctx context.Context, repoRoot string) (rebuildGTSyncOutcome, string) {
	lockPath := filepath.Join(d.config.TownRoot, "daemon", "install-gt.lock")
	if dir := os.Getenv("INSTALL_GT_DAEMON_DIR"); dir != "" {
		lockPath = filepath.Join(dir, "install-gt.lock")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return rebuildGTSyncDefer, "cannot create the install lock directory: " + err.Error()
	}

	lockCtx, cancel := context.WithTimeout(ctx, rebuildGTLockWait)
	defer cancel()
	lock := flock.New(lockPath)
	locked, err := lock.TryLockContext(lockCtx, 250*time.Millisecond)
	if err != nil || !locked {
		return rebuildGTSyncDefer, "another install holds the install lock past " + rebuildGTLockWait.String()
	}
	defer func() { _ = lock.Unlock() }()

	// The fetch is best-effort: a checkout that cannot reach origin still gets
	// the fast-forward attempted against the ref it has.
	_, _ = d.rebuildGTGit(repoRoot, "fetch", "origin", "--quiet")
	if _, err := d.rebuildGTGit(repoRoot, "merge", "--ff-only", "origin/main", "--quiet"); err != nil {
		// rev-list --count prints a number, "0" included: a non-zero count is
		// local commits origin/main does not have, which is a real divergence
		// and never to be reset away; zero means the fast-forward failed for
		// some other reason (an untracked file it would overwrite, say) that a
		// retry can still clear (gt-9jax).
		if count, cerr := d.rebuildGTGit(repoRoot, "rev-list", "origin/main..HEAD", "--count"); cerr == nil {
			if n, perr := strconv.Atoi(strings.TrimSpace(count)); perr == nil && n > 0 {
				return rebuildGTSyncRefuse, "local main diverged from origin/main"
			}
		}
		d.logger.Printf("rebuild_gt: origin/main moved during the sync; re-fetching and re-merging once")
		_, _ = d.rebuildGTGit(repoRoot, "fetch", "origin", "--quiet")
		if _, err := d.rebuildGTGit(repoRoot, "merge", "--ff-only", "origin/main", "--quiet"); err != nil {
			return rebuildGTSyncRefuse, "local main diverged from origin/main"
		}
	}
	return rebuildGTSyncOK, ""
}

// rebuildGTInstall runs the town's one install path and maps its result.
// reserveTimeout is the seconds the install may wait for a container-gate
// slot; zero takes no slot.
func (d *Daemon) rebuildGTInstall(ctx context.Context, cycle *dogCycle, repoRoot string, reserveTimeout int) bool {
	installer := filepath.Join(repoRoot, "scripts", "install-gt.sh")
	if _, err := os.Stat(installer); err != nil {
		cycle.failStep("installer", "scripts/install-gt.sh is missing")
		_ = d.rebuildGTAlert(alertKeyRebuildGTNoInstaller,
			fmt.Sprintf("rebuild_gt: %s is missing, so nothing can install gt", installer))
		return true
	}

	head, err := d.rebuildGTGit(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		cycle.skipStep("install", "cannot resolve the source HEAD: "+err.Error())
		d.logger.Printf("rebuild_gt: cannot resolve HEAD in %s (%v); deferring", repoRoot, err)
		return false
	}
	head = strings.TrimSpace(head)

	args := []string{installer, "--sha", head, "--source", rebuildGTSource}
	if reserveTimeout > 0 {
		args = append(args, "--slot-role", rebuildGTSlotRole, "--slot-timeout", strconv.Itoa(reserveTimeout))
	}

	out := &rebuildGTInstallLog{}
	cmd := exec.CommandContext(ctx, "bash", args...)
	cmd.Dir = repoRoot
	// SKIP_UPDATE_CHECK stays inside install-gt.sh; the environment carries
	// only the two knobs the plugin set (gt-9jax).
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("INSTALL_GT_LOCK_WAIT=%d", int(rebuildGTInstallLockWait.Seconds())),
		"INSTALL_GT_RIG_DIR="+repoRoot,
	)
	cmd.Stdout = out
	cmd.Stderr = out

	d.logger.Printf("rebuild_gt: running %s --sha %s --source %s (this builds and can take minutes)", installer, shortSHA(head), rebuildGTSource)
	_, _, runErr := d.runCmd(cmd)
	res := out.result()
	d.logger.Printf("rebuild_gt: install-gt finished (%s); last lines:\n%s", res.event, out.tail(rebuildGTLogTailLines))

	code := 0
	if runErr != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	switch {
	case code == 0 && res.event == "noop":
		d.rebuildGTReachedForce(cycle, "the binary already contains "+shortSHA(head))
		return true
	case code == 0:
		from, to := res.prev, res.commit
		if from == "" {
			from = shortSHA(head)
		}
		if to == "" {
			to = shortSHA(head)
		}
		subjects := d.rebuildGTSubjects(repoRoot, from, to)
		cycle.closeStep("install")
		d.logger.Printf("rebuild_gt: in force %s -> %s; the daemon restarts at its idle point\n%s", from, to, subjects)
		d.rebuildGTReachedForce(cycle, "the binary is in force")
		return true
	case code == 2:
		cycle.skipStep("install", "install-gt refused: "+reasonOr(res.reason, "unknown"))
		d.rebuildGTNoteBlocked(cycle, true, "install-gt refused: "+reasonOr(res.reason, "unknown"))
		return true
	case code == 3:
		// A slot or lock the install could not take: nothing accomplished,
		// retry next heartbeat.
		d.rebuildGTNoteBlocked(cycle, true, "install-gt deferred: "+reasonOr(res.reason, "unknown"))
		d.logger.Printf("rebuild_gt: install-gt deferred (%s); retrying next heartbeat", reasonOr(res.reason, "unknown"))
		return false
	default:
		reason := reasonOr(res.reason, fmt.Sprintf("exit %d", code))
		cycle.failStep("install", "install-gt failed: "+reason)
		// install-gt escalates its own build, smoke, rollback and marker
		// failures under install-gt:*; anything else — its unexpected trap, a
		// missing rig, or no RESULT line at all — reached nobody.
		switch res.reason {
		case "build-failed", "smoke-failed", "rollback-failed", "marker-write":
		default:
			_ = d.rebuildGTAlert(alertKeyRebuildGTInstallFail,
				fmt.Sprintf("rebuild_gt: install-gt failed (%s) installing %s", reason, shortSHA(head)))
		}
		return true
	}
}

// rebuildGTInstallLog collects the installer's output, for the RESULT line and
// for the tail the daemon logs when the run ends. A run that waited minutes on
// a slot prints a start line and a tail rather than streaming, so a quiet log
// during the build is not evidence of a hang (gt-kox0).
type rebuildGTInstallLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *rebuildGTInstallLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// tail returns the last n lines of what the installer printed.
func (w *rebuildGTInstallLog) tail(n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := strings.Split(strings.TrimRight(w.buf.String(), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// rebuildGTInstallResult is install-gt.sh's one machine-readable line,
// `install-gt: RESULT <event> <commit> <prev> <reason>`, where a field it has
// nothing to say about is "-".
type rebuildGTInstallResult struct {
	event  string
	commit string
	prev   string
	reason string
}

func (w *rebuildGTInstallLog) result() rebuildGTInstallResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	var res rebuildGTInstallResult
	for _, line := range strings.Split(w.buf.String(), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || fields[0] != "install-gt:" || fields[1] != "RESULT" {
			continue
		}
		// Every field is one token: install-gt writes a code (build-failed,
		// lock-busy, no-rig) or "-" for a field it has nothing to say about.
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

// rebuildGTSubjects lists the commits a successful install brought into force:
// that range is the inert window, so the receipt answers "was this fix ever in
// force, and when" without reconstructing it from merge timestamps.
func (d *Daemon) rebuildGTSubjects(repoRoot, from, to string) string {
	if from == "" || to == "" || from == to {
		return ""
	}
	out, err := d.rebuildGTGit(repoRoot, "log", "--no-decorate", "--oneline", from+".."+to)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > rebuildGTSubjectLimit {
		lines = append(lines[:rebuildGTSubjectLimit], fmt.Sprintf("... and %d more", len(lines)-rebuildGTSubjectLimit))
	}
	return strings.Join(lines, "\n")
}

// rebuildGTStale reads binary staleness in-process. `gt stale` is this same
// call; running it here keeps the job free of a subprocess and of the JSON's
// own ambiguities.
func (d *Daemon) rebuildGTStale(repoRoot string) (*version.StaleBinaryInfo, bool) {
	check := d.rebuildGTStaleFn
	if check == nil {
		check = version.CheckStaleBinaryFresh
	}
	info := check(repoRoot)
	if info == nil || info.Error != nil || info.Skipped {
		return nil, false
	}
	return info, true
}

// rebuildGTAlert raises a MEDIUM alert for this job under key: the conditions
// that want an operator's attention rather than the town stopping. The
// delivery outcome is exposed because a caller that retries on a drop must
// know it happened.
func (d *Daemon) rebuildGTAlert(key, message string) error {
	return d.escalateAlertSeverity("MEDIUM", key, rebuildGTSource, message)
}

// rebuildGTDrift escalates on the outcome that matters — the binary falling
// behind origin/main — before any skip reason is decided. A count that could
// not be taken is not 0: it could be 1 commit or 1000, so it escalates too
// (gt-oqbw).
func (d *Daemon) rebuildGTDrift(info *version.StaleBinaryInfo) {
	if !info.IsStale {
		return
	}
	switch {
	case info.CommitsBehind > rebuildGTMaxCommitsBehind:
		_ = d.rebuildGTAlert(alertKeyRebuildGTDrift,
			fmt.Sprintf("rebuild-gt: binary is %d commits behind origin/main and has not been rebuilt", info.CommitsBehind))
	case info.CommitsBehind == 0:
		_ = d.rebuildGTAlert(alertKeyRebuildGTDriftUnknown,
			"rebuild-gt: binary is stale but commits_behind could not be determined")
	}
}

// rebuildGTNoteBlocked records a cycle that did not install the binary and,
// when the binary was due, opens (or ages) the block against it and escalates
// once that block outlasts rebuildGTStarveAfter. A skip that leaves a binary
// needing no install out of force is not a block.
func (d *Daemon) rebuildGTNoteBlocked(cycle *dogCycle, due bool, reason string) {
	cycle.skipStep("blocked", reason)
	if !due {
		d.logger.Printf("rebuild_gt: %s", reason)
		return
	}
	now := d.clk().Now()
	age := d.rebuildGTBlock.open(now)
	d.logger.Printf("rebuild_gt: due and not installed for %s: %s", age.Round(time.Minute), reason)
	if age < rebuildGTStarveAfter || !d.rebuildGTBlock.alertOnce() {
		return
	}
	if err := d.rebuildGTAlert(alertKeyRebuildGTStarved,
		fmt.Sprintf("rebuild-gt: the binary has been due for install and blocked for %s; last: %s",
			age.Round(time.Minute), reason)); err != nil {
		// The escalation never reached the town: let the next cycle retry
		// rather than record a firing nobody saw (gt-kox0).
		d.logger.Printf("rebuild_gt: starvation escalation did not reach the town; retrying next cycle: %v", err)
		d.rebuildGTBlock.mu.Lock()
		d.rebuildGTBlock.alerted = false
		d.rebuildGTBlock.mu.Unlock()
	}
}

// rebuildGTNotDue closes the starvation block: the binary needs nothing
// installed, so nothing is starving. The alarm follows the state — a block
// left open past the condition it measured outlives it (gt-kox0, gt-vwry).
// detail names the reading that closed it.
func (d *Daemon) rebuildGTNotDue(cycle *dogCycle, detail string) {
	cycle.skipStep("threshold", detail)
	if _, open := d.rebuildGTBlock.openFor(d.clk().Now()); !open {
		return
	}
	d.rebuildGTBlock.clear()
	d.clearAlerts(detail, alertKeyRebuildGTStarved)
}

// rebuildGTReachedForce records that the binary is fresh or installed, and
// closes every alarm this job owns: each of them asserts the binary is out of
// force somewhere, and once it is in force those assertions are false.
func (d *Daemon) rebuildGTReachedForce(cycle *dogCycle, reason string) {
	d.rebuildGTBlock.clear()
	cycle.closeStep("force")
	d.logger.Printf("rebuild_gt: %s", reason)
	d.clearAlerts("the binary is in force",
		alertKeyRebuildGTStarved, alertKeyRebuildGTDrift, alertKeyRebuildGTDriftUnknown,
		alertKeyRebuildGTUnverified, alertKeyRebuildGTNoInstaller, alertKeyRebuildGTInstallFail)
}

// rebuildGTGate reads the container-gate picture: which gate-class roles hold
// a slot, and whether the blocker is one the slot serializes (and so can be
// waited out). A reading that fails is not a reason to block: a broken status
// read must never park installs forever.
func (d *Daemon) rebuildGTGate() (busy string, reservable bool) {
	if d.rebuildGTGateFn != nil {
		return d.rebuildGTGateFn()
	}
	rep, err := slot.StatusPool(d.config.TownRoot, slot.PoolForTown(d.config.TownRoot))
	if err != nil {
		d.logger.Printf("rebuild_gt: slot status unreadable (%v); treating the town as quiet", err)
		return "", false
	}
	var holders []string
	for _, s := range rep.Slots {
		if s.Held && s.Owner != nil && slot.IsGateRole(s.Owner.Role) {
			holders = append(holders, s.Owner.Role)
		}
	}
	saturated := rep.AllHeld()
	switch {
	case len(holders) > 0:
		busy = "a gate suite holds a slot (" + strings.Join(holders, ", ") + ")"
	case len(rep.UnwrappedContainers) > 0:
		busy = "container(s) are running outside the gate: " + strings.Join(rep.UnwrappedContainers, ", ")
	case saturated:
		busy = "every container-gate slot is held"
	default:
		// A docker reading that could not be taken is not a reason to wait: a
		// VM that is down runs no suite to compete with.
		return "", false
	}
	return busy, len(holders) > 0 || saturated
}

// rebuildGTWaitForGate blocks until no gate-class role holds a slot, or the
// reserve budget runs out. The poll is what the acquire alone cannot do: on a
// multi-slot pool a free slot goes to a non-gate caller immediately, so the
// build would start beside the very suite the yield exists to stay out of
// (gt-htx3, gt-kox0).
func (d *Daemon) rebuildGTWaitForGate(ctx context.Context, deadline time.Time) bool {
	for {
		if busy, _ := d.rebuildGTGate(); busy == "" {
			return true
		}
		if !d.clk().Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-d.clk().After(rebuildGTGatePoll):
		}
	}
}

// rebuildGTGit runs one git command in the source checkout, returning stdout.
func (d *Daemon) rebuildGTGit(repoRoot string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rebuildGTGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	stdout, stderr, err := d.runCmd(cmd)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = err.Error()
		}
		return string(stdout), errors.New(msg)
	}
	return string(stdout), nil
}

func dashToEmpty(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
