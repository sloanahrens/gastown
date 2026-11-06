package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/buildpaths"
	"github.com/steveyegge/gastown/internal/installpending"
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
// install` (gt-oqbw). This job decides; the Go installer (cmd/gt-installer)
// installs.
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

	// defaultRebuildGTMinInterval is the least time between two installs a
	// landing's plain request may force, measured from the last accomplished
	// cycle: more than one build-and-restart per 30 minutes cut dispatch ticks
	// mid-sling (gt-aiby1).
	defaultRebuildGTMinInterval = 30 * time.Minute

	// rebuildGTInstallThreshold is how many commits behind the binary must be
	// to be due. One merged commit not in force is a live defect, not a
	// rounding error: this job is the backstop for landings nobody installed.
	rebuildGTInstallThreshold = 1

	// rebuildGTMaxCommitsBehind is where a due binary stops being merely due
	// and becomes drift worth an operator's attention.
	rebuildGTMaxCommitsBehind = 20

	// rebuildGTReserveWait bounds the wait for a gate to release its slot,
	// together with the acquire that follows it.
	rebuildGTReserveWait = 10 * time.Minute

	// rebuildGTMinReserve is the floor on the slot wait handed to the installer
	// once the reserve budget is spent; a zero would disable its own wait.
	rebuildGTMinReserve = time.Minute

	// rebuildGTGatePoll is how often the reserve wait re-reads the slot.
	rebuildGTGatePoll = 15 * time.Second

	// rebuildGTLockWait is how long the sync waits for the install lock
	// before deferring to the next heartbeat: a lock still held means an
	// install is running right now.
	rebuildGTLockWait = 30 * time.Second

	// rebuildGTInstallLockWait is what the installer waits for the same lock.
	// A minute, not its own five: a longer wait only keeps this job running,
	// which keeps the daemon non-idle and delays its upgrade restart.
	rebuildGTInstallLockWait = 60 * time.Second

	// rebuildGTRunBudget bounds one cycle: the reserve wait, both lock waits,
	// and the build and install behind them.
	rebuildGTRunBudget = 25 * time.Minute

	// rebuildGTGitTimeout bounds one git call.
	rebuildGTGitTimeout = 2 * time.Minute

	// rebuildGTRigSyncTimeout bounds one rig checkout's sync: its fetch, its
	// history reads and its fast-forward together, so a rig whose remote is slow
	// cannot hold the cycle open.
	rebuildGTRigSyncTimeout = 60 * time.Second

	// rebuildGTRigSyncBudget bounds the whole rig sweep, so a town with many
	// slow rigs cannot hold the cycle open either.
	rebuildGTRigSyncBudget = 5 * time.Minute

	// rebuildGTSubjectLimit caps the commit subjects the install records.
	rebuildGTSubjectLimit = 20

	// rebuildGTLogTailLines is how much of the installer's output the daemon
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
	alertKeyRebuildGTMarkerWrite  = "rebuild-gt:marker-write"
	alertKeyRebuildGTMarkerClear  = "rebuild-gt:marker-clear"
	alertKeyRebuildGTInstallRec   = "rebuild-gt:install-record"

	// alertKeyRebuildGTUnverified is retired (nothing raises it) and kept in
	// the clear list because an escalation opened before it was retired has no
	// other closer.
	alertKeyRebuildGTUnverified = "rebuild-gt:unverified"
)

// rebuildGTSource is the --source every escalation and install from this job
// carries, the same string the plugin used.
const rebuildGTSource = "rebuild-gt"

// rebuildGTSourceRig is the one rig whose canonical checkout rebuildGTSync
// fast-forwards before an install. The rig sweep skips it: that checkout has
// already been moved by the time the sweep runs (gt-f91o8).
const rebuildGTSourceRig = "gastown"

// rebuildGTInstallRecord is the last-run key recording the last install that
// built and swapped a binary, which is what the install window is measured
// from. The patrol's own last-run record is written on every settled verdict,
// so a cycle that found nothing due, was refused, failed, or installed a noop
// anchored a window over a build that never happened (gt-t135z).
const rebuildGTInstallRecord = "rebuild_gt_install"

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

func rebuildGTMinInterval(config *DaemonPatrolConfig) time.Duration {
	if config == nil || config.Patrols == nil {
		return defaultRebuildGTMinInterval
	}
	return config.Patrols.RebuildGT.MinIntervalOrDefault(defaultRebuildGTMinInterval)
}

// rebuildGTRecordInstall records an install that built and swapped a binary, in
// the same last-run file and under the same lock as the patrol's own record.
// It is the only writer of the install window's anchor: a refusal, a failure, a
// noop and a deferral all leave it where it was (gt-t135z).
// A write that fails leaves the window unanchored, so every later request
// builds and swaps the binary again; that reaches an operator rather than a log
// line alone (gt-2bk8s).
func (d *Daemon) rebuildGTRecordInstall(at time.Time) {
	if err := savePatrolLastRun(d.config.TownRoot, rebuildGTInstallRecord, at); err != nil {
		d.logger.Printf("rebuild_gt: WARNING: cannot persist the install time (%v)", err)
		_ = d.rebuildGTAlert(alertKeyRebuildGTInstallRec,
			fmt.Sprintf("rebuild_gt: cannot write %s, so the install window does not start and the next request builds the binary again", patrolLastRunPath(d.config.TownRoot)))
		return
	}
	d.clearAlerts("the install time is recorded", alertKeyRebuildGTInstallRec)
}

// rebuildGTInstallAge returns how long ago the last accomplished install was,
// and false when none was recorded or the record cannot be read. Nothing holds
// a request back on state it could not read: the request exists to get a binary
// installed (gt-t135z).
func (d *Daemon) rebuildGTInstallAge(now time.Time) (time.Duration, bool) {
	at, found, err := loadPatrolLastRun(d.config.TownRoot, rebuildGTInstallRecord)
	if err != nil || !found {
		return 0, false
	}
	return now.Sub(at), true
}

// updateTriggerEnabled reports whether the update job owns the install and
// maintenance decisions: patrols.update parses and is enabled. rebuild_gt and
// the maintenance window tick read this one helper, so they step aside
// together, and switching the feature off restores both old triggers.
func (d *Daemon) updateTriggerEnabled() bool {
	settings, ok := d.updateSettings()
	return ok && settings.Enabled
}

// triggerRebuildGT runs one rebuild_gt cycle on its own goroutine when the
// job is due: an install builds the whole binary and can wait minutes for a
// container-gate slot, so inline it would stall the heartbeat.
func (d *Daemon) triggerRebuildGT() {
	if !d.isPatrolActive("rebuild_gt") {
		return
	}
	// When the update job owns the install decision this job makes none: two
	// deciders would race the same binary. The latch below logs the line once
	// per daemon process, not once per heartbeat, whatever the setting does.
	if d.updateTriggerEnabled() {
		if d.rebuildGTDeferredLogged.CompareAndSwap(false, true) {
			d.logger.Printf("rebuild_gt: install decisions are deferred to the update job")
		}
		return
	}
	now := d.clk().Now()
	armed, installNow := d.rebuildGTPendingRequest()
	lastRun, found, loadErr := loadPatrolLastRun(d.config.TownRoot, "rebuild_gt")
	dec := patrolDueFromLastRun(lastRun, found, loadErr, "", now, rebuildGTInterval(d.patrolConfig))
	// A request armed by a landing pass makes the job due on its own: an
	// install is the point of the drain signal, and the interval it would
	// otherwise wait out is what leaves the merge inert (gt-3qmv4.1).
	if !dec.due && !armed {
		return
	}
	// A plain request waits out the install window: landings drain in bursts,
	// and one build per burst is the point (gt-aiby1). A gt:install-now
	// landing bypasses it, and the interval-due path is unchanged.
	//
	// The window runs from the last install that built and swapped a binary
	// (rebuildGTInstallAge), not from the patrol's last settled verdict: that
	// record is written for a cycle that found nothing due, was refused,
	// failed, or installed a noop, so it anchored the window over a build that
	// never happened (gt-t135z). An absent or unreadable install record holds
	// nothing back, the safe direction for a request whose purpose is to get a
	// binary installed. A request held inside the window stays armed and logs
	// once per wait.
	if armed && !installNow && !dec.due {
		window := rebuildGTMinInterval(d.patrolConfig)
		if age, installed := d.rebuildGTInstallAge(now); installed && age < window {
			if d.rebuildGTWindowLogged.CompareAndSwap(false, true) {
				d.logger.Printf("rebuild_gt: install requested %s after the last install; waiting for the %s window", age.Round(time.Second), window)
			}
			return
		}
	}
	if !d.rebuildGTRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.due && dec.warn != "" {
		d.logger.Printf("rebuild_gt: WARNING: %s — %s", dec.warn, dec.note)
	}
	d.rebuildGTWindowLogged.Store(false)
	go func() {
		// Registered first so it runs last, after the guard clears: a test
		// woken by it reads the cycle's own end, not an earlier cycle's
		// (gt-v0t6k).
		defer d.noteRebuildGTCycleDone()
		defer d.rebuildGTRunning.Store(false)
		// The cycle consumes the request as it starts, so one armed while it
		// runs sets the flag for the next heartbeat; clearing at the end
		// would erase that request too (gt-j1mm4).
		consumed, consumedInstallNow := d.takeRebuildGTRequest()
		accomplished := false
		// Put back the request this cycle consumed unless it accomplished its
		// work, so a deferral, a panic or a Goexit does not lose it. Registered
		// after the guard clears, so it runs before the guard does: the next
		// heartbeat sees the request (gt-tgs81).
		defer func() {
			if consumed && !accomplished {
				d.putBackRebuildGTRequest(consumedInstallNow)
			}
		}()
		if !d.runRebuildGT() {
			// Nothing accomplished: leave the gate open so the next
			// heartbeat retries (the script's exit 3).
			return
		}
		accomplished = true
		if err := savePatrolLastRun(d.config.TownRoot, "rebuild_gt", d.clk().Now()); err != nil {
			d.logger.Printf("rebuild_gt: WARNING: cannot persist last-run time (%v)", err)
		}
	}()
}

// noteRebuildGTCycleDone tells a test its wait for a cycle is over (see
// rebuildGTCycleDoneFn). It runs on the cycle's goroutine, so the hook must not
// block: a test that stopped waiting cannot stall the cycle (gt-v0t6k).
func (d *Daemon) noteRebuildGTCycleDone() {
	if d.rebuildGTCycleDoneFn != nil {
		d.rebuildGTCycleDoneFn()
	}
}

// requestRebuildGTInstall arms the sticky request that makes the next
// rebuild_gt heartbeat due regardless of the patrol's interval. A cycle takes
// it when it starts and puts it back unless the cycle accomplishes its work,
// so a deferral or an exit keeps the install pending and a request made
// mid-cycle survives it; arming an
// already-armed request is a no-op, which is what keeps the log one line per
// drain rather than one per landing pass (gt-3qmv4.1).
func (d *Daemon) requestRebuildGTInstall() {
	if d.armRebuildGTRequest(false) {
		d.logger.Printf("rebuild_gt: install requested: queue drained after landing")
	}
}

// requestRebuildGTInstallNow arms the same request for a bead the operator
// labeled gt:install-now: its landing asks for the install at once, so the
// request is not held back for the queue to drain (gt-3qmv4.2), nor for the
// minimum install interval a plain landing request waits out (gt-aiby1).
func (d *Daemon) requestRebuildGTInstallNow() {
	if d.armRebuildGTRequest(true) {
		d.logger.Printf("rebuild_gt: install requested: a %s bead landed", landworker.LabelInstallNow)
	}
}

// rebuildGTPendingRequest reports the pending request: whether one is armed,
// and whether it bypasses the install window. The two are read together, so a
// request arriving concurrently is never seen half-set.
func (d *Daemon) rebuildGTPendingRequest() (armed, installNow bool) {
	d.rebuildGTRequestMu.Lock()
	defer d.rebuildGTRequestMu.Unlock()
	return d.rebuildGTRequested.Load(), d.rebuildGTInstallNow.Load()
}

// armRebuildGTRequest arms the request, upgrading one that is already pending
// to an install-now one when this landing carried the label: the operator
// asked for the install at once, and the pending request has not run yet.
// Reports whether this call armed the request, so the log stays one line per
// drain.
//
// An install-now marker is only ever stored beside an armed request, under the
// same lock the cycle takes the pair with, so it cannot be left behind for a
// later plain request to inherit.
func (d *Daemon) armRebuildGTRequest(installNow bool) (armed bool) {
	d.rebuildGTRequestMu.Lock()
	defer d.rebuildGTRequestMu.Unlock()
	if installNow {
		d.rebuildGTInstallNow.Store(true)
	}
	return d.rebuildGTRequested.CompareAndSwap(false, true)
}

// takeRebuildGTRequest consumes the pending request as a cycle starts, both
// flags in one critical section: a request arriving while the cycle runs must
// not lose the bypass it carried to this cycle's clear, nor leave one behind
// once this request's bypass has been served.
func (d *Daemon) takeRebuildGTRequest() (armed, installNow bool) {
	d.rebuildGTRequestMu.Lock()
	defer d.rebuildGTRequestMu.Unlock()
	armed = d.rebuildGTRequested.Swap(false)
	installNow = d.rebuildGTInstallNow.Swap(false)
	return armed, armed && installNow
}

// putBackRebuildGTRequest re-arms a request a cycle consumed without
// accomplishing its work, keeping the bypass it carried: a gt:install-now
// request asked for the install at once, and a deferral (a busy gate, a held
// lock) must not push it behind the install window the label skips. A plain
// request goes back plain. The lock keeps a request armed meanwhile from
// losing the bypass it carries, and a bypass armed on top of the put-back
// request is left in place.
func (d *Daemon) putBackRebuildGTRequest(installNow bool) {
	d.rebuildGTRequestMu.Lock()
	defer d.rebuildGTRequestMu.Unlock()
	if installNow {
		d.rebuildGTInstallNow.Store(true)
	}
	d.rebuildGTRequested.Store(true)
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
		d.rebuildGTClearPending()
		return true
	}

	settled := d.rebuildGTCycle(ctx, cycle, repoRoot, now)
	if settled {
		// A verdict leaves no install in flight: the marker is spent whether
		// the cycle installed, refused or found nothing due, and a deferral is
		// the only outcome that keeps it standing for the next heartbeat
		// (gt-vc34c).
		d.rebuildGTClearPending()
	}
	return settled
}

// rebuildGTCycle is the script's flow, in its order.
func (d *Daemon) rebuildGTCycle(ctx context.Context, cycle *dogCycle, repoRoot string, now time.Time) (settled bool) {
	// The rig sweep runs last and only for a cycle that reached a verdict after
	// the gt sync: the deferred call lands below every return in this function,
	// so no rig can delay the install above it. A deferral syncs nothing,
	// because the next heartbeat runs the whole cycle again and would fetch
	// every rig a second time to reach the same place (gt-f91o8).
	//
	// It runs on the cycle's own goroutine — the one triggerRebuildGT launches
	// only after rebuildGTRunning.CompareAndSwap(false, true) — and never on the
	// heartbeat's, which returned when that goroutine started. What it costs is
	// time, not the heartbeat: while the sweep runs (at most
	// rebuildGTRigSyncBudget) the guard stays set, so a landing's install
	// request that arrives meanwhile is not served by this cycle. It stays
	// armed — a cycle consumes a request only as it starts
	// (takeRebuildGTRequest) and puts back one it did not accomplish — and the
	// first heartbeat after the sweep takes it. One settled cycle fetches about
	// eight remotes (every rig but gastown); its cadence is the rebuild_gt
	// patrol interval (rebuildGTInterval, one hour unless daemon.json sets
	// another), so no cooldown is needed unless that interval is under a minute.
	synced := false
	defer func() {
		if settled && synced {
			d.rebuildGTSyncRigCheckouts()
		}
	}()

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
	d.rebuildGTDrift(repoRoot, before)

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
	synced = true

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

	// A backlog that changed nothing `make build` reads installs nothing: the
	// binary already produces everything that change can. This runs after the
	// staleness and safety checks and before the quiet gate, so a docs- or
	// test-only landing costs neither a container-gate slot nor the daemon
	// restart an install ends in (gt-3qmv4.3). A reading this cannot take is
	// binary-affecting: an unreadable diff is not evidence that nothing needs
	// building.
	if nonBinary, read := d.rebuildGTNonBinaryRange(repoRoot, after.BinaryCommit, after.RepoCommit); read && nonBinary {
		cycle.skipStep("non-binary", "only non-binary paths changed")
		d.logger.Printf("rebuild_gt: skip: only non-binary paths changed (%d commits)", after.CommitsBehind)
		// Nothing is due, so the starvation clock closes rather than ages: the
		// whole range is non-binary, so no earlier binary-affecting commit is
		// waiting in it.
		d.rebuildGTCloseBlock("only non-binary paths changed")
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

	// The install is due and safe: record it before the wait below, which can
	// hold for minutes of gate polling or a starved reserve, so a reader that
	// would start work beside the install can see it coming (gt-vc34c).
	//
	// A cycle that defers keeps the marker, and the next heartbeat refreshes
	// it, so a gate that stays busy holds the marker up past Ceiling: the
	// starvation escalation at rebuildGTInstallCap is what bounds that
	// standing, because it is the alarm for a due binary that never installs.
	d.rebuildGTMarkPending(now)

	// Yield to a running gate: `make build` competes for CPU with a suite
	// whose tests are load-sensitive, and the install itself takes a slot when
	// it has waited one out (gt-htx3, gt-kox0).
	//
	// reserveTimeout is seconds the install may wait for the slot once it runs.
	// Zero means it does not take one at all.
	reserveTimeout := 0
	waited := false
	if busy, reservable := d.rebuildGTGate(); busy != "" {
		age, _ := d.rebuildGTBlock.openFor(now)
		switch {
		case reservable && age >= d.rebuildGTInstallCap():
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
			waited = true
		default:
			d.rebuildGTNoteBlocked(cycle, true, "not quiet: "+busy)
			return false
		}
	}

	// The wait above is unbounded time inside one cycle: Ceiling is aged against
	// the marker's own time, so a cycle that has waited must move it or readers
	// stop seeing an install in flight while it still runs (gt-vc34c).
	if waited {
		d.rebuildGTMarkPending(d.clk().Now())
	}

	d.logger.Printf("rebuild_gt: installing %s (%s commits behind) through cmd/gt-installer", repoRoot, behind)
	return d.rebuildGTInstall(ctx, cycle, repoRoot, reserveTimeout)
}

// rebuildGTNonBinaryPath reports whether a repo-relative path can be changed
// without changing what `make build` produces. The rule is buildpaths'
// AffectsBuild, which `gt stale` reads too, so this job and the staleness
// warning the user sees cannot disagree about what an install would produce
// (gt-p62ku).
func rebuildGTNonBinaryPath(p string) bool {
	return !buildpaths.AffectsBuild(p)
}

// rebuildGTNonBinaryRange reports whether every path changed in from..to is
// non-binary. read is false when git could not list the changes, or listed
// none for a range that is stale: neither is evidence that nothing needs
// building, and both send the caller on to install.
func (d *Daemon) rebuildGTNonBinaryRange(repoRoot, from, to string) (nonBinary, read bool) {
	if from == "" || to == "" {
		return false, false
	}
	out, err := d.rebuildGTGit(repoRoot, "diff", "--name-only", from+".."+to)
	if err != nil {
		d.logger.Printf("rebuild_gt: cannot list the changes in %s..%s (%v); installing", shortSHA(from), shortSHA(to), err)
		return false, false
	}
	paths := strings.Fields(out)
	if len(paths) == 0 {
		return false, false
	}
	for _, p := range paths {
		if !rebuildGTNonBinaryPath(p) {
			return false, true
		}
	}
	return true, true
}

// rebuildGTLogFormat puts the record separator, then the commit hash, at the
// head of each commit's record in the listing rebuildGTBinaryAffectingCount
// reads.
const rebuildGTLogFormat = "--format=%x1f%H"

// rebuildGTBinaryAffectingCount counts the commits in from..to that change a
// path `make build` reads, so a backlog of documentation is not drift. Each
// commit's paths are classified by buildpaths.AffectsBuild, the predicate
// `gt stale` counts with, so markdown under an embed directory is drift just
// as it is there (gt-9euk3). ok is false when git could not answer.
func (d *Daemon) rebuildGTBinaryAffectingCount(repoRoot, from, to string) (int, bool) {
	if from == "" || to == "" {
		return 0, false
	}
	out, err := d.rebuildGTGit(repoRoot, "log", rebuildGTLogFormat, "--name-only", "--diff-merges=first-parent", from+".."+to)
	if err != nil {
		return 0, false
	}
	return rebuildGTCountAffectingRecords(out), true
}

// rebuildGTCountAffectingRecords counts the records of a rebuildGTLogFormat
// listing in which at least one path is binary-affecting. Each record is the
// commit hash on its first line, then the paths the commit changed.
func rebuildGTCountAffectingRecords(out string) int {
	affecting := 0
	for _, record := range strings.Split(out, "\x1f") {
		for _, path := range strings.Split(record, "\n")[1:] {
			// An empty line is not a path, and AffectsBuild("") is true.
			if path = strings.TrimSpace(path); path != "" && buildpaths.AffectsBuild(path) {
				affecting++
				break
			}
		}
	}
	return affecting
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
// the install lock. Every write to that tree happens under this lock, so
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

	// A fetch that failed leaves origin/main where it was, so the merge below is
	// a no-op and the staleness read after it compares the binary against the
	// checkout's own stale ref: a checkout that is itself behind origin then
	// reads as fresh and closes the starvation alarm over a binary that is
	// stale. Nothing was accomplished, so retry next heartbeat (gt-oyrav).
	if _, err := d.rebuildGTGit(repoRoot, "fetch", "origin", "--quiet"); err != nil {
		return rebuildGTSyncDefer, "cannot fetch origin: " + err.Error()
	}
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
		if _, err := d.rebuildGTGit(repoRoot, "fetch", "origin", "--quiet"); err != nil {
			return rebuildGTSyncDefer, "cannot re-fetch origin: " + err.Error()
		}
		if _, err := d.rebuildGTGit(repoRoot, "merge", "--ff-only", "origin/main", "--quiet"); err != nil {
			return rebuildGTSyncRefuse, "local main diverged from origin/main"
		}
	}
	return rebuildGTSyncOK, ""
}

// rebuildGTRigCheckoutCounts is what one rig sweep did, for the cycle's log
// line. Moved + Skipped + Failed is every rig the sweep considered: each known
// rig but gastown, except one with no canonical checkout to move.
type rebuildGTRigCheckoutCounts struct {
	Moved, Skipped, Failed int
}

// rebuildGTRigDir is one rig whose canonical checkout the sweep will visit.
type rebuildGTRigDir struct {
	rig string
	dir string
}

// rebuildGTRigNameOK reports whether rig can be joined under the town root: an
// empty name, "." or ".." would name the town root itself, and a separator
// would name a directory that is not the rig (gt-f91o8).
func rebuildGTRigNameOK(rig string) bool {
	if rig == "" || rig == "." || rig == ".." {
		return false
	}
	return !strings.ContainsAny(rig, `/\`)
}

// rebuildGTSyncRigCheckouts fast-forwards the canonical checkout of every rig
// but gastown, <town>/<rig>/mayor/rig, to origin/main.
//
// A rig's directive layer and its bead redirect are read from that tree, so a
// directive landed on a rig's main stays invisible until its checkout moves,
// and nothing else ever moved one (gt-f91o8).
//
// This is the only code that fetches in another rig's canonical checkout: the
// install and the staleness read fetch in gastown's own, which they build from
// and compare the binary against, and sessions and crews fetch in none they did
// not open (docs/reference.md states the rule). The sweep runs after the cycle
// has reached its verdict, so a slow or failing rig never delays or fails the
// gt install, and every rig it does not move is logged once with its reason and
// left alone.
func (d *Daemon) rebuildGTSyncRigCheckouts() rebuildGTRigCheckoutCounts {
	budget := rebuildGTRigSyncBudget
	if d.rebuildGTRigSyncBudgetFn != nil {
		budget = d.rebuildGTRigSyncBudgetFn()
	}
	// The daemon's own context is the parent, so a daemon stop ends the sweep
	// between rigs and cuts the git call it is in (shutdown cancels d.ctx before
	// the process exits). The cycle's context is not used: it is built from
	// Background with the install's budget, which the sweep must not inherit.
	parent := context.Background()
	if d.ctx != nil {
		parent = d.ctx
	}
	budgetCtx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	// Cloned before sorting: getKnownRigs hands back the daemon's cached slice,
	// which readers on other goroutines hold.
	rigs := slices.Clone(d.getKnownRigs())
	slices.Sort(rigs)
	var counts rebuildGTRigCheckoutCounts

	// Which rigs have a checkout is settled before any git runs, so the counts
	// cover every checkout in the town: a rig the budget never reaches is
	// skipped with that reason rather than dropped from the cycle's line
	// (gt-f91o8).
	var checkouts []rebuildGTRigDir
	for _, rig := range rigs {
		if rig == rebuildGTSourceRig {
			continue
		}
		if !rebuildGTRigNameOK(rig) {
			counts.Skipped++
			d.logger.Printf("rebuild_gt: rig %q: invalid rig name; leaving its checkout alone", rig)
			continue
		}
		checkout := filepath.Join(d.config.TownRoot, rig, "mayor", "rig")
		if _, err := os.Stat(filepath.Join(checkout, ".git")); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// No canonical checkout: nothing to move.
				continue
			}
			counts.Failed++
			d.logger.Printf("rebuild_gt: rig %s: cannot read its checkout (%v); leaving it alone", rig, err)
			continue
		}
		checkouts = append(checkouts, rebuildGTRigDir{rig: rig, dir: checkout})
	}

	for i, rig := range checkouts {
		if budgetCtx.Err() != nil {
			// The budget is spent before these rigs were visited: they are
			// skipped, not dropped, so the counts still add up to every
			// checkout the town has (gt-f91o8).
			for _, rest := range checkouts[i:] {
				counts.Skipped++
				if d.ctx != nil && d.ctx.Err() != nil {
					d.logger.Printf("rebuild_gt: rig %s: the daemon is stopping; leaving its checkout alone", rest.rig)
					continue
				}
				d.logger.Printf("rebuild_gt: rig %s: the %s sweep budget is spent; leaving its checkout alone", rest.rig, budget)
			}
			break
		}
		// The bound is asked with the rig, so a test can hold one rig short
		// and every other generous: the outcome each rig gets then depends on
		// the rig, never on how fast the machine is (gt-gg9hv.51).
		timeout := rebuildGTRigSyncTimeout
		if d.rebuildGTRigSyncTimeoutFn != nil {
			timeout = d.rebuildGTRigSyncTimeoutFn(rig.rig)
		}
		rigCtx, rigCancel := context.WithTimeout(budgetCtx, timeout)
		moved, failed, why := d.rebuildGTSyncRigCheckout(rigCtx, rig.dir)
		rigCancel()
		switch {
		case moved:
			counts.Moved++
		case failed:
			counts.Failed++
			d.logger.Printf("rebuild_gt: rig %s: %s; leaving its checkout alone", rig.rig, why)
		default:
			counts.Skipped++
			d.logger.Printf("rebuild_gt: rig %s: %s; leaving its checkout alone", rig.rig, why)
		}
	}
	d.logger.Printf("rebuild_gt: rig checkouts: %d moved, %d skipped, %d failed", counts.Moved, counts.Skipped, counts.Failed)
	return counts
}

// rebuildGTSyncRigCheckout fast-forwards one rig's canonical checkout to
// origin/main and reports what it did: moved, or failed, or skipped with why.
//
// Only a checkout on main, clean, and strictly behind origin/main is moved: a
// fast-forward is the only write here, and it is the only write that cannot
// lose anything. Every other state is a tree someone may be holding on
// purpose, so it is reported and left alone.
func (d *Daemon) rebuildGTSyncRigCheckout(ctx context.Context, checkout string) (moved, failed bool, why string) {
	// git is this checkout's runner, bounded by the caller's context: the seam
	// carries that context so a test can drive the per-rig bound with a runner
	// that starts no process (gt-f91o8).
	git := d.rebuildGTGitCtx
	if d.rebuildGTRigGitFn != nil {
		git = d.rebuildGTRigGitFn
	}
	branch, err := git(ctx, checkout, "branch", "--show-current")
	if err != nil {
		return false, true, "cannot read the branch: " + err.Error()
	}
	if b := strings.TrimSpace(branch); b != "main" {
		if b == "" {
			return false, false, "HEAD is detached"
		}
		return false, false, "on branch " + b
	}

	// .beads is excluded for the reason the gastown pre-flight excludes it: the
	// checkout holds tracked bead state that changes without anyone touching the
	// tree, and refusing to move over that would hold the rig back forever
	// (gt-50k). A change there that the fast-forward would overwrite fails the
	// merge below, which is reported.
	dirty, err := git(ctx, checkout, "status", "--porcelain", "--untracked-files=no", "--", ".", ":(exclude).beads")
	if err != nil {
		return false, true, "cannot read the work tree: " + err.Error()
	}
	if strings.TrimSpace(dirty) != "" {
		return false, false, "the work tree has uncommitted changes"
	}

	if _, err := git(ctx, checkout, "fetch", "origin", "--quiet"); err != nil {
		return false, true, "cannot fetch origin: " + err.Error()
	}

	// One read answers both questions the fast-forward rests on: rev-list
	// --left-right --count HEAD...origin/main prints "<ahead>\t<behind>", so
	// ahead == 0 is exactly "HEAD is an ancestor of origin/main" and behind == 0
	// is "there is nothing to fast-forward".
	out, err := git(ctx, checkout, "rev-list", "--left-right", "--count", "HEAD...origin/main")
	if err != nil {
		return false, true, "cannot compare HEAD with origin/main: " + err.Error()
	}
	ahead, behind, ok := parseAheadBehind(out)
	if !ok {
		return false, true, "cannot compare HEAD with origin/main: rev-list did not print two counts"
	}
	if ahead > 0 {
		if ahead == 1 {
			return false, false, "1 commit on HEAD is not on origin/main"
		}
		return false, false, fmt.Sprintf("%d commits on HEAD are not on origin/main", ahead)
	}
	if behind == 0 {
		return false, false, "already at origin/main"
	}

	if _, err := git(ctx, checkout, "merge", "--ff-only", "origin/main", "--quiet"); err != nil {
		return false, true, "cannot fast-forward to origin/main: " + err.Error()
	}
	return true, false, ""
}

// parseAheadBehind reads the "<ahead>\t<behind>" rev-list --left-right --count
// prints. ok is false on anything else, which is a reading to report, never a
// zero to act on.
func parseAheadBehind(out string) (ahead, behind int, ok bool) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, false
	}
	a, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return a, b, true
}

// rebuildGTInstall builds the Go installer from the synced checkout, runs it
// and maps its result. reserveTimeout is the seconds the install may wait for
// a container-gate slot; zero takes no slot.
//
// The installer is built and then started, never run through `go run`: go run
// ends with its own status whatever the program exited with, and the mapping
// below reads the installer's 0-3.
func (d *Daemon) rebuildGTInstall(ctx context.Context, cycle *dogCycle, repoRoot string, reserveTimeout int) bool {
	installer := filepath.Join(repoRoot, "cmd", "gt-installer")
	if _, err := os.Stat(installer); err != nil {
		cycle.failStep("installer", "cmd/gt-installer is missing")
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

	// The Makefile's target is the build `make install` does, so a human's
	// installer and this job's are one program built one way (gt-tpg0p.14).
	buildOut := &rebuildGTInstallLog{}
	buildCmd := exec.CommandContext(ctx, "make", "gt-installer")
	buildCmd.Dir = repoRoot
	buildCmd.Stdout = buildOut
	buildCmd.Stderr = buildOut
	if _, _, buildErr := d.runCmd(buildCmd); buildErr != nil {
		if ctx.Err() != nil {
			d.logger.Printf("rebuild_gt: the installer build was killed with the cycle (%v); retrying next heartbeat", ctx.Err())
			d.rebuildGTNoteBlocked(cycle, true, "the installer build was killed with the cycle")
			return false
		}
		cycle.failStep("installer", "make gt-installer failed")
		d.logger.Printf("rebuild_gt: make gt-installer failed (%v); last lines:\n%s", buildErr, buildOut.tail(rebuildGTLogTailLines))
		_ = d.rebuildGTAlert(alertKeyRebuildGTInstallFail,
			fmt.Sprintf("rebuild_gt: cannot build the installer in %s", repoRoot))
		return true
	}
	program := filepath.Join(repoRoot, "gt-installer")

	args := []string{"--sha", head, "--source", rebuildGTSource}
	if reserveTimeout > 0 {
		args = append(args, "--slot-role", slot.InstallRole, "--slot-timeout", strconv.Itoa(reserveTimeout))
	}

	out := &rebuildGTInstallLog{}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = repoRoot
	// The environment carries the two knobs the plugin set (gt-9jax) and,
	// when the sweep has an engine of its own, the context the polecat-image
	// plugin trims beside the default one (gt-bxp17).
	cmd.Env = d.withPolecatImageTrimContext(append(os.Environ(),
		fmt.Sprintf("INSTALL_GT_LOCK_WAIT=%d", int(rebuildGTInstallLockWait.Seconds())),
		"INSTALL_GT_RIG_DIR="+repoRoot,
	))
	cmd.Stdout = out
	cmd.Stderr = out

	d.logger.Printf("rebuild_gt: running %s --sha %s --source %s (this builds and can take minutes)", program, shortSHA(head), rebuildGTSource)
	_, _, runErr := d.runCmd(cmd)
	res := out.result()
	d.logger.Printf("rebuild_gt: the installer finished (%s); last lines:\n%s", res.event, out.tail(rebuildGTLogTailLines))

	// A program that could not be started never ran, so it was not killed and
	// it reported nothing. The next heartbeat would fail the same way, so this
	// escalates rather than deferring.
	var execErr *exec.Error
	var pathErr *os.PathError
	if runErr != nil && ctx.Err() == nil && (errors.As(runErr, &execErr) || errors.As(runErr, &pathErr)) {
		cycle.failStep("installer", "the installer did not start")
		_ = d.rebuildGTAlert(alertKeyRebuildGTInstallFail,
			fmt.Sprintf("rebuild_gt: cannot start the installer %s: %v", program, runErr))
		return true
	}

	code := 0
	if runErr != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	// A run the cycle's own budget killed, or one a signal killed, never printed
	// its RESULT line: the fields read here describe no install at all, and
	// taking them as a verdict would hold the hour over work that never
	// happened. Deferring returns the request this cycle consumed (see the
	// cycle goroutine in triggerRebuildGT) and the next heartbeat retries. This
	// is not an installer failure to escalate: the kill is the daemon's own
	// clock, and the starvation block below is what escalates a binary that
	// stays uninstalled (gt-oyrav).
	if ctx.Err() != nil || code < 0 {
		d.logger.Printf("rebuild_gt: the installer was killed before it reported a result (ctx %v, exit %d); retrying next heartbeat", ctx.Err(), code)
		d.rebuildGTNoteBlocked(cycle, true, "the installer was killed before it reported a result")
		return false
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
		// This is the one outcome that built and swapped a binary, so it is the
		// one that starts the install window (gt-t135z).
		d.rebuildGTRecordInstall(d.clk().Now())
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

// rebuildGTInstallResult is the installer's one machine-readable line,
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
// behind origin/main — before any skip reason is decided. The count is of
// binary-affecting commits only, so a backlog of documentation, markdown outside
// the embed directories, test sources and testdata never escalates: it is not a
// binary anybody is running behind (gt-3qmv4.3). A count that could not be taken is not 0: it could be 1
// commit or 1000, so it escalates too (gt-oqbw).
func (d *Daemon) rebuildGTDrift(repoRoot string, info *version.StaleBinaryInfo) {
	if !info.IsStale {
		return
	}
	if info.CommitsBehind == 0 {
		_ = d.rebuildGTAlert(alertKeyRebuildGTDriftUnknown,
			"rebuild-gt: binary is stale but commits_behind could not be determined")
		return
	}
	if info.CommitsBehind <= rebuildGTMaxCommitsBehind {
		// Under the ceiling by the raw count, which can only overcount: the
		// binary-affecting count is no larger, so there is nothing to say.
		return
	}
	count, ok := d.rebuildGTBinaryAffectingCount(repoRoot, info.BinaryCommit, info.RepoCommit)
	if !ok {
		// The range could not be subdivided. Fall back to the path reading: a
		// range that changed nothing the binary is built from is not drift,
		// and one that could not be read either keeps the raw count rather
		// than dropping the alarm.
		if nonBinary, read := d.rebuildGTNonBinaryRange(repoRoot, info.BinaryCommit, info.RepoCommit); read && nonBinary {
			count = 0
		} else {
			count = info.CommitsBehind
		}
	}
	if count > rebuildGTMaxCommitsBehind {
		_ = d.rebuildGTAlert(alertKeyRebuildGTDrift,
			fmt.Sprintf("rebuild-gt: binary is %d commits behind origin/main and has not been rebuilt", count))
	}
}

// rebuildGTNoteBlocked records a cycle that did not install the binary and,
// when the binary was due, opens (or ages) the block against it and escalates
// once that block outlasts rebuildGTInstallCap. A skip that leaves a binary
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
	if age < d.rebuildGTInstallCap() || !d.rebuildGTBlock.alertOnce() {
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
	d.rebuildGTCloseBlock(detail)
}

// rebuildGTCloseBlock closes the starvation block, if one is open, under the
// reading that closed it.
func (d *Daemon) rebuildGTCloseBlock(detail string) {
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

// rebuildGTPendingStore is this town's pending-install marker.
func (d *Daemon) rebuildGTPendingStore() installpending.Store {
	if d.rebuildGTPendingFn != nil {
		return d.rebuildGTPendingFn()
	}
	return installpending.StoreFor(d.config.TownRoot)
}

// rebuildGTMarkPending records that a binary-affecting install is due, for
// readers outside this job. A marker that cannot be written costs those
// readers their warning, not the install: the cycle carries on, and the failure
// is escalated rather than logged alone, because a reader with no marker reads
// exactly what a town with no install reads (gt-vc34c).
func (d *Daemon) rebuildGTMarkPending(now time.Time) {
	store := d.rebuildGTPendingStore()
	if err := store.Write(now, os.Getpid()); err != nil {
		d.logger.Printf("rebuild_gt: WARNING: cannot record the pending install (%v)", err)
		_ = d.rebuildGTAlert(alertKeyRebuildGTMarkerWrite,
			fmt.Sprintf("rebuild_gt: cannot write %s, so a reader cannot see an install in flight", store.Path()))
		return
	}
	d.clearAlerts("the pending-install marker is written", alertKeyRebuildGTMarkerWrite)
}

// rebuildGTClearPending drops the pending-install marker: the cycle reached a
// verdict, so no install is in flight for a reader to avoid (gt-vc34c). A
// marker that cannot be removed is escalated rather than logged alone: it keeps
// telling every later reader an install is in flight long after this one
// finished, and only an operator can take the debris away.
func (d *Daemon) rebuildGTClearPending() {
	store := d.rebuildGTPendingStore()
	if err := store.Clear(); err != nil {
		d.logger.Printf("rebuild_gt: WARNING: cannot clear the pending install marker (%v)", err)
		_ = d.rebuildGTAlert(alertKeyRebuildGTMarkerClear,
			fmt.Sprintf("rebuild_gt: cannot remove %s, so a reader keeps seeing an install in flight", store.Path()))
		return
	}
	d.clearAlerts("the pending-install marker is cleared", alertKeyRebuildGTMarkerClear)
}

// rebuildGTInstallCap is how long a due binary may go uninstalled before the
// block is escalated and, when the container-gate slot is the blocker, waited
// for instead of raced. Under it, a busy gate is the town working as designed
// (gate holds measured 2m-20m20s on 2026-09-22); past it, the block is the
// thing to report.
//
// It is the install cap of the pool this town runs: a post-landing acquisition
// yields to an install in flight for at most Pool.InstallYield, and this job
// raises the alarm at that same duration. Reading the pool rather than copying
// its default keeps the two together if the cap ever moves (gt-pa4tg).
func (d *Daemon) rebuildGTInstallCap() time.Duration {
	if d.rebuildGTInstallYieldFn != nil {
		return d.rebuildGTInstallYieldFn()
	}
	return slot.PoolForTown(d.config.TownRoot).InstallYield()
}

// rebuildGTGate reads the container-gate picture: which gate-class roles hold
// a slot, and whether the blocker is one the slot serializes (and so can be
// waited out).
//
// Three readings of the report are this job's own:
//
//   - the documented scratch project's containers are a known holder, so a
//     running L1/L2 window does not block an install for its whole hour. They
//     are named in the log on every path, whether or not something else blocks,
//     because a reader of the log otherwise cannot tell what was on the VM
//     (gt-4gixm);
//   - the project mid-work — a container docker is creating or restarting, or a
//     build for it in flight — does block: an image build and a Docker restart
//     are what this check protects (gt-4gixm);
//   - a docker or gate reading that could not be taken is not quiet. Only the
//     daemon refusing its own connection means nothing can be running, and that
//     one case still proceeds; anything else refuses the install and says so,
//     because an unreadable VM is not an empty one.
func (d *Daemon) rebuildGTGate() (busy string, reservable bool) {
	if d.rebuildGTGateFn != nil {
		return d.rebuildGTGateFn()
	}
	// slotGate is the host gate unless a test supplied one over a fake runtime;
	// the production path is the same classification either way (gt-4gixm).
	g := d.slotGate
	if g == nil {
		g = slot.NewGate()
	}
	rep, err := g.StatusPool(d.config.TownRoot, slot.PoolForTown(d.config.TownRoot))
	if err != nil {
		d.logger.Printf("rebuild_gt: not quiet: the container-gate status could not be read (%v)", err)
		return "the container-gate status could not be read", false
	}

	// Every held pool slot is named as its seat, not just gate roles: the
	// readings below reach the MEDIUM starvation escalation, and naming who
	// holds the slots is what makes that alarm actionable (gt-t135z). A
	// marker's row is not a pool slot, so it is skipped.
	var gateSeats, heldSeats []string
	for _, s := range rep.Slots {
		if !s.Held || s.Marker {
			continue
		}
		heldSeats = append(heldSeats, slotSeat(s))
		if s.Owner != nil && slot.IsGateRole(s.Owner.Role) {
			gateSeats = append(gateSeats, slotSeat(s))
		}
	}
	saturated := rep.AllHeld()

	// Named before the decision, so it is on the record even when something
	// else is what blocked this pass (gt-4gixm).
	if len(rep.ScratchContainers) > 0 {
		d.logger.Printf("rebuild_gt: proceeding despite the documented scratch project's containers: %s (a known holder, not an unwrapped suite)",
			strings.Join(rep.ScratchContainers, ", "))
	}

	switch {
	case len(gateSeats) > 0:
		busy = "a gate suite holds a slot (" + strings.Join(gateSeats, ", ") + ")"
	case len(rep.ScratchBuilding) > 0:
		busy = "the documented scratch project is being built or restarted: " + strings.Join(rep.ScratchBuilding, ", ") +
			" (an image build or a Docker restart is what this check protects)"
	case rep.DockerUnknown && !rep.DockerQuiet():
		// Docker could not be read for a reason other than its own daemon
		// refusing the connection (a wedged daemon, a socket this process cannot
		// open): the VM is unread, not quiet, and an install must not walk into
		// what it cannot see.
		d.logger.Printf("rebuild_gt: not quiet: the docker container listing could not be read (%s)", rep.DockerError)
		busy = "the docker container listing could not be read, so the VM's containers are unknown"
	case len(rep.UnwrappedContainers) > 0:
		busy = "container(s) are running outside the gate: " + strings.Join(rep.UnwrappedContainers, ", ")
	case rep.DockerUnknown:
		// Docker's own message is that it cannot connect to its daemon: no
		// container is running to compete with the build.
		d.logger.Printf("rebuild_gt: treating the town as quiet: docker's daemon refused the connection (%s)", rep.DockerError)
	case saturated:
		busy = "every container-gate slot is held (" + strings.Join(heldSeats, ", ") + ")"
	}
	return busy, len(gateSeats) > 0 || saturated
}

// slotSeat renders one held slot's holder as the escalations name it: the role,
// the pid and the slot, e.g. "gastown/landing pid 123 (slot 0)". A held slot
// with no readable owner file names the slot alone, because the flock is the
// truth and the owner file is decoration.
func slotSeat(s slot.SlotState) string {
	if s.Owner == nil || s.Owner.PID <= 0 {
		return fmt.Sprintf("unowned (slot %d)", s.Index)
	}
	return fmt.Sprintf("%s pid %d (slot %d)", s.Owner.Role, s.Owner.PID, s.Index)
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
	return d.rebuildGTGitCtx(context.Background(), repoRoot, args...)
}

// rebuildGTGitCtx is rebuildGTGit under a caller's context, so a caller that
// bounds a whole checkout's sync as one step (rather than one call at a time)
// stops every git call under it when that bound expires.
func (d *Daemon) rebuildGTGitCtx(ctx context.Context, repoRoot string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, rebuildGTGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	stdout, stderr, err := d.runCmd(cmd)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = err.Error()
		}
		// A git killed by the bound reports the signal, which says nothing about
		// why: name the expiry instead (gt-f91o8).
		if ctx.Err() != nil {
			msg = "git " + strings.Join(args, " ") + ": " + ctx.Err().Error()
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
