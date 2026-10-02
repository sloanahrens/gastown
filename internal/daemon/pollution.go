package daemon

import (
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/procid"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmuxsweep"
	"github.com/steveyegge/gastown/internal/util"
)

// Test-pollution sweep (gt-4k3fj.6.2).
//
// The retired deacon's test-pollution-cleanup step is the last of the patrol
// steps that littered without a Go owner. It becomes one job on the doctor
// dog's cadence, covering what the doctor dog's own checks do not:
//
//   - orphaned embedded `dolt sql-server` processes and the temp directories
//     of a killed beads suite (gt-twil, moved here from doctor_dog.go);
//   - an imposter Dolt holding the town's port;
//   - dead session and nudge-poller PID files under .runtime;
//   - the tmux servers a killed suite left bound to gt-test-* sockets.
//
// Every part is a fact about a process, so no part needs judgement, and none
// reads a failed measurement as absence: a sweep that cannot take a
// measurement reports it and moves on rather than acting on the unknown.
//
// The deacon's fourth residual, an unpushed-aware prune of orphaned dog
// worktrees, is not here: the dog kennel was retired (gt-ckunw) and nothing
// in the tree creates a worktree under it, so there is no litter to reap
// (docs/adr/0005-patrol-scan-tick.md).

// runTestPollutionSweep runs every part of the sweep and logs what it did.
func (d *Daemon) runTestPollutionSweep() {
	d.reapOrphanDoltTestServers()
	d.sweepImposterDolt()
	d.sweepDeadPIDFiles()
	d.sweepTestTmuxServers()
}

// reapOrphanDoltTestServers reaps orphaned test 'dolt sql-server' processes:
// leftovers from an embedded-dolt test suite killed at its timeout, whose
// shared/per-test server never got torn down and was reparented to
// init/launchd. An 18h-old orphan survived beads' own test-side reaper, so the
// town needs its own guard on the doctor dog cadence (gt-twil).
func (d *Daemon) reapOrphanDoltTestServers() {
	if d.seams.orphanReap != nil {
		d.seams.orphanReap()
		return
	}
	orphans, err := util.FindOrphanDoltServers(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("Warning: dolt orphan server scan failed: %v", err)
		return
	}

	for _, o := range orphans {
		if o.Reason != "orphan" {
			d.logger.Printf("dolt orphan server scan: unexpected dolt sql-server PID %d ppid=%d (%s) — not auto-reaped",
				o.PID, o.PPID, o.ConfigPath)
		}
	}

	if results := util.ReapOrphanDoltServers(orphans); len(results) > 0 {
		d.logger.Printf("dolt orphan server cleanup: reaped %d process(es)", len(results))
		for _, r := range results {
			if r.Error != nil {
				d.logger.Printf("  WARNING: SIGTERM PID %d (%s) failed: %v", r.Process.PID, r.Process.ConfigPath, r.Error)
			} else {
				d.logger.Printf("  Sent SIGTERM to PID %d ppid=%d: %s", r.Process.PID, r.Process.PPID, r.Process.ConfigPath)
			}
		}
	}

	stale, err := util.FindStaleBeadsTestTempDirs()
	if err != nil {
		d.logger.Printf("Warning: stale beads test temp dir scan failed: %v", err)
		return
	}
	if len(stale) == 0 {
		return
	}
	removed, err := util.RemoveStaleBeadsTestTempDirs(stale)
	if len(removed) > 0 {
		d.logger.Printf("dolt orphan server cleanup: removed %d stale beads-bd-tests-* temp dir(s)", len(removed))
	}
	if err != nil {
		d.logger.Printf("Warning: failed to remove some stale test temp dirs: %v", err)
	}
}

// sweepImposterDolt reaps a foreign Dolt holding the town's port — the same
// decision `gt dolt kill-imposters` makes, on the sweep's cadence. The
// daemon's identity check already covers this while it manages the server
// (dolt.go); a town running an external Dolt has no such path.
func (d *Daemon) sweepImposterDolt() {
	if d.seams.imposterSweep != nil {
		d.seams.imposterSweep(d.config.TownRoot)
		return
	}
	pid, dataDir := doltserver.CheckPortConflict(d.config.TownRoot)
	if pid == 0 {
		return
	}
	d.logger.Printf("test_pollution: Dolt port conflict: PID %d holds the port serving %q", pid, dataDir)
	// KillImposters re-verifies that the holder is a dolt sql-server before
	// signaling it: a port holder that is not provably Dolt — Docker Desktop
	// fronts every published container port — is skipped, never killed
	// (gt-p7zy0).
	if err := doltserver.KillImposters(d.config.TownRoot); err != nil {
		d.logger.Printf("test_pollution: killing imposter Dolt server (PID %d): %v", pid, err)
	}
}

// sweepDeadPIDFiles removes the PID file of a session or poller whose process
// is gone. A session that dies without UntrackPID leaves its file behind, and
// the only sweep that reclaimed one runs during `gt down`, so on a town that
// never goes down they accumulate — one per polecat that ever ran.
func (d *Daemon) sweepDeadPIDFiles() {
	if rep, err := session.PruneDeadTrackedPIDs(d.config.TownRoot); err != nil {
		d.logger.Printf("test_pollution: scanning .runtime/pids: %v", err)
	} else {
		d.logPrunedRecords("session", rep)
	}
	if rep, err := nudge.PruneDeadPollerPIDFiles(d.config.TownRoot); err != nil {
		d.logger.Printf("test_pollution: scanning .runtime/nudge_poller: %v", err)
	} else {
		d.logPrunedRecords("nudge-poller", rep)
	}
}

// logPrunedRecords reports one PID-file sweep. It names what it removed only
// when it removed something: an all-clear line every five minutes for the life
// of the daemon is noise. A record it could not act on is always named.
func (d *Daemon) logPrunedRecords(kind string, rep procid.PruneReport) {
	if rep.Removed > 0 {
		d.logger.Printf("test_pollution: removed %d dead %s PID file(s), kept %d live", rep.Removed, kind, rep.Kept)
	}
	for _, problem := range rep.Problems {
		d.logger.Printf("test_pollution: %s PID file %s", kind, problem)
	}
}

// sweepTestTmuxServers finds and reaps the tmux servers a killed test run left
// behind. Their sessions are test-named, which the roster reads as phantom
// polecats with no worktree and no agent bead (gt-2bj).
func (d *Daemon) sweepTestTmuxServers() {
	report, err := d.seams.sweepTestSockets()
	if err != nil {
		d.logger.Printf("test_pollution: scanning test tmux sockets: %v", err)
		return
	}
	if len(report.Unprobed) > 0 {
		// A pass the sweep did not earn is the report it must not give.
		d.logger.Printf("test_pollution: %d tmux test socket(s) could not be probed", len(report.Unprobed))
	}
	if report.Found() {
		d.logger.Printf("test_pollution: reaped %d abandoned test tmux server(s), removed %d socket file(s)",
			len(report.Leftovers), len(report.StaleFiles))
	}
}

// sweepTestSockets is the production sweep behind the seam: scan the machine's
// socket directory, then reap whatever the scan found.
func sweepTestSockets() (tmuxsweep.Report, error) {
	report, err := tmuxsweep.Scan(tmuxsweep.Options{})
	if err != nil || !report.Found() {
		return report, err
	}
	return report, tmuxsweep.Reap(tmuxsweep.Options{}, report)
}
