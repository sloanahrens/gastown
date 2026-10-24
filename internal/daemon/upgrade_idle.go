package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/version"
)

// isIdleForUpgrade reports whether restarting the daemon now would kill no
// in-flight work: no script plugin, compactor, boot triage, scheduled
// slings or patrol watchdog run, no landing-worker pass, no paid Jev pass
// (jev_dedupe, jev_attention, jev_landing, jev_submission, jev_dispatch), no
// scheduled_maintenance gc cycle, and no
// install holding install-gt.lock. The Dolt goroutines are not counted: they
// are short or restartable. Post-landing runs and tier sweep cycles are not
// here either: checkUpgradeRestart holds a bounded wait for each instead
// (postLandRestartCap, tierSweepRestartCap), and a predicate that never
// cleared would hold a restart forever. The maintenance gc cycle is the one
// hold both places carry: the flag here, and the drain's bounded wait
// (maintenanceGCRestartCap) in upgradeHold (gt-oucgn).
//
// checkUpgradeRestart reads the named form instead — upgradeHold, which is
// this predicate with each hold's cap and the wait line's name on it
// (gt-rtbbr) — so this stays the plain yes/no the predicate is defined as.
func (d *Daemon) isIdleForUpgrade() bool {
	if d.maintenanceGCRunning.Load() {
		return false
	}
	return d.daemonWorkIdle()
}

// daemonWorkIdle is isIdleForUpgrade without the gc cycle's own flag: whoever
// asks accounts for the cycle separately (upgradeHold waits it out at
// maintenanceGCRestartCap), so this reads only what else is in flight. The gc's
// own quiet guard needs no such flag — it asks the hard holds alone
// (maintenanceQuiet, through catchUpWorkHold).
func (d *Daemon) daemonWorkIdle() bool {
	return d.daemonWorkHold() == nil
}

// daemonWork is one piece of daemon work in flight: what the upgrade restart's
// wait line calls it, and whether a restart must wait for it at all.
//
// Hard work is work a restart would destroy with nothing to redo it: a landing
// pass owns the merged-tree gate of the bead it is landing (gt-u641b), and
// the install lock means the binary is mid-replacement, so a restart inside
// that window execs one its own smoke check has not verified. Everything else
// — plugin runs, dog cycles, the daemon's own ticks and scans — reruns from the
// new daemon, so it holds a restart only up to housekeepingRestartCap. That the
// two sets differ is only visible when a restart is pending; the list itself is
// shared, because what makes the daemon busy and what an operator is told it is
// busy with must not drift apart (gt-rtbbr).
type daemonWork struct {
	name string
	hard bool
}

// daemonWorkHold reports the piece of daemon work that holds the daemon: the
// first hard hold in flight when there is one, else the first hold of any kind,
// else nil. Hard work is never masked by soft work that sorts earlier in
// daemonWorkHolds (gt-rtbbr): a plugin run and a held install lock in flight
// together must name the lock and wait for it, never let the restart cut over
// it at housekeepingRestartCap.
func (d *Daemon) daemonWorkHold() *daemonWork {
	holds := d.daemonWorkHolds()
	for _, hold := range holds {
		if hold.hard {
			return hold
		}
	}
	if len(holds) == 0 {
		return nil
	}
	return holds[0]
}

// catchUpWorkHold reports the daemon work a catch-up backup must wait for: the
// hard hold in flight, else nil (gt-2m8sj).
//
// A backup is a read through the running server, so soft work — a plugin run,
// a dog cycle, a dispatch tick, a scan — is no conflict with it and must not
// defer it. Counting that work starved the catch-up: every check in a run-loop
// pass shares a start time with the dispatch and patrol-scan tickers, so five
// consecutive checks deferred with a soft hold in flight and the newest backup
// reached 62h. The hard holds and a held container-gate slot (catchUpQuiet)
// still defer it.
func (d *Daemon) catchUpWorkHold() *daemonWork {
	for _, hold := range d.daemonWorkHolds() {
		if hold.hard {
			return hold
		}
	}
	return nil
}

// daemonWorkHolds lists the daemon work in flight, in the order the wait line
// names it when none of it is hard. It is the one list behind daemonWorkIdle,
// the upgrade restart's wait line and the gc's quiet guard (catchUpWorkHold);
// daemonWorkHold picks the hard-first entry from it.
//
// The install-lock probe is last and only runs when nothing else, or only soft
// work, is in flight. It is the one check that touches the filesystem
// (daemonWorkHold is called on every upgrade heartbeat, catchUpWorkHold on
// every gc quiet check), and a hard hold already in hand decides
// daemonWorkHold's answer whatever the probe would say.
func (d *Daemon) daemonWorkHolds() []*daemonWork {
	var holds []*daemonWork
	if names := d.scripts.inFlight(); len(names) > 0 {
		holds = append(holds, &daemonWork{name: "a plugin run " + names[0]})
	}
	d.compactorDogMu.Lock()
	compactor := d.compactorDogRunning
	d.compactorDogMu.Unlock()
	if compactor {
		holds = append(holds, &daemonWork{name: "the compactor dog cycle"})
	}
	// The in-flight landing is named before the counter is read: a pass that
	// has begun but has no bead in flight yet (landingStates.beginPass clears
	// it) is still a hold, and the counter is what says so.
	if bead := d.landingPassBead(); bead != "" {
		holds = append(holds, &daemonWork{name: landingHoldName(bead), hard: true})
	} else if d.landingPasses.Load() > 0 {
		holds = append(holds, &daemonWork{name: landingHoldName(""), hard: true})
	}
	if d.scheduledSlingsRunning.Load() {
		// A cycle is a dispatcher: a cut mid-entry can leave a run bead created
		// and unslung, which is not a rerun of the same work. It holds as it
		// always has, with the 30m escalation as its only bound.
		holds = append(holds, &daemonWork{name: "a scheduled slings cycle", hard: true})
	}
	if d.specDispatchRunning.Load() {
		holds = append(holds, &daemonWork{name: specDispatchHoldLabel})
	}
	// The slings a tick handed back run on after it (gt-5h70s); a restart that
	// cuts one leaves its seat hooked with no session, as a cut tick did
	// (gt-cbaoy).
	if _, slinging := d.specSlings.oldest(); slinging {
		holds = append(holds, &daemonWork{name: specSlingHoldLabel})
	}
	if d.patrolScanRunning.Load() {
		holds = append(holds, &daemonWork{name: "a patrol scan"})
	}
	// A restart kills the Jev call in flight before its line is recorded, and
	// the next pass asks and bills that finding again (gt-0h4i0). The pass
	// reruns from the new daemon, so it is soft work. The other paid passes
	// lose the same call the same way (gt-tevxz, gt-dlnkz).
	for _, pass := range []struct {
		running *atomic.Bool
		name    string
	}{
		{&d.jevDedupeRunning, "jev_dedupe"},
		{&d.jevAttentionRunning, "jev_attention"},
		{&d.jevLandingRunning, "jev_landing"},
		{&d.jevSubmissionRunning, "jev_submission"},
		{&d.jevDispatchRunning, "jev_dispatch"},
		{&d.jevThrashRunning, "jev_thrash"},
		{&d.jevRulesRunning, "jev_rules"},
		{&d.jevConflictRunning, "jev_conflict"},
		{&d.jevDriftRunning, "jev_drift"},
		{&d.jevLogtriageRunning, "jev_logtriage"},
		{&d.jevReworkRunning, "jev_rework"},
		{&d.jevProgressRunning, "jev_progress"},
		{&d.jevNotesRunning, "jev_notes"},
		{&d.jevStaleRunning, "jev_stale"},
	} {
		if pass.running.Load() {
			holds = append(holds, &daemonWork{name: "a " + pass.name + " pass"})
		}
	}
	for _, hold := range holds {
		if hold.hard {
			return holds
		}
	}
	if d.installLockHeld() {
		holds = append(holds, &daemonWork{name: "the install lock (an install is replacing the binary)", hard: true})
	}
	return holds
}

// landingHoldName names an in-flight landing pass: the bead being landed when
// one is in flight, else the pass itself.
func landingHoldName(bead string) string {
	if bead == "" {
		return "a landing pass"
	}
	return "landing pass " + bead
}

// installLockPath is scripts/install-gt.sh's lock:
// ${INSTALL_GT_DAEMON_DIR:-$TOWN_ROOT/daemon}/install-gt.lock. The daemon
// never sets INSTALL_GT_DAEMON_DIR, so its town's daemon dir is the one.
func installLockPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "install-gt.lock")
}

// installLockHeld reports whether an install-gt.sh run holds its lock. An
// install replaces the binary before its smoke check; restarting inside that
// window would exec an unverified binary that a failed smoke check then rolls
// back underneath the daemon. The probe never blocks: it opens the existing
// file (never creating it), tries a non-blocking exclusive flock and releases
// at once. A missing file means no install has ever run: not held. Any other
// error is treated as held, the safe side (restart-pending-stuck escalates if
// it persists).
func (d *Daemon) installLockHeld() bool {
	path := installLockPath(d.config.TownRoot)
	// SetFlag replaces flock's default O_CREATE|O_RDONLY: never create it.
	lock := flock.New(path, flock.SetFlag(os.O_RDONLY))
	locked, err := lock.TryLock()
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		d.logger.Printf("upgrade: cannot probe %s: %v; treating the install lock as held", path, err)
		return true
	}
	if !locked {
		return true
	}
	_ = lock.Unlock()
	return false
}

// buildCommit is the daemon's own build commit: buildCommitFn's when a test
// set one, else the version package's.
func (d *Daemon) buildCommit() string {
	if d.buildCommitFn != nil {
		return d.buildCommitFn()
	}
	return version.BuildCommit()
}

// resolveOwnCommit returns the full SHA of this build when the gastown source
// repo can resolve it, else the (short) build commit.
func (d *Daemon) resolveOwnCommit() string {
	own := d.buildCommit()
	if own == "" {
		return ""
	}
	repo := filepath.Join(d.config.TownRoot, "gastown", "mayor", "rig")
	full, err := d.gitAt(repo).Rev(own + "^{commit}")
	if err != nil {
		return own
	}
	return strings.TrimSpace(full)
}
