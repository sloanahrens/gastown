package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrRestartForUpgrade is returned by Run after a normal shutdown when the
// daemon exits so launchd restarts it on a newly installed binary. The
// caller maps it to exit code 75: launchd's KeepAlive {SuccessfulExit: false}
// restarts only a nonzero exit.
var ErrRestartForUpgrade = errors.New("daemon: restart for upgrade")

// upgradeStuckAfter is how long a newer marker may wait for an idle heartbeat
// before the daemon escalates (it keeps waiting afterwards).
const upgradeStuckAfter = 30 * time.Minute

// postLandRestartCap is how long a pending upgrade restart waits for an
// in-flight post-landing run (gt-gb4ij). Past the cap the daemon restarts
// anyway, and the landing worker's first pass after the start runs the
// untested tip again (Worker.watchTarget), so a cut run costs only time.
// Short on purpose (gt-8p8h7): the drain starts no landing pass while it waits,
// and every landing's install requests a restart, so a 20m cap left the
// queue idle ~8 min after landings for a shell-test run worth less than
// the landings it held up. The cap is not in
// isIdleForUpgrade: it needs the marker's wait time, and the gc cycle's quiet
// check already sees the run through its gate-class container slot.
const postLandRestartCap = 2 * time.Minute

// tierSweepRestartCap is how long a pending upgrade restart waits for an
// in-flight tier sweep cycle (gt-ccyw0). Restarting during one kills it and
// loses its verdict, so the new daemon runs the cycle again: on 2026-10-04 an
// install cut an even-hour integration cycle 5.5 minutes in, and the restart
// cost ~9 minutes of rerun. The sweep's own budget is tierSweepRunBudget (2h),
// too long to hold an install, so past the cap the daemon restarts anyway and
// says it cut the sweep short. The cap is not in isIdleForUpgrade: it needs
// the marker's wait time, and a sweep in the idle predicate would hold the
// restart forever.
const tierSweepRestartCap = 15 * time.Minute

// housekeepingRestartCap is how long a pending upgrade restart waits for the
// daemon's own housekeeping — a script plugin run, a dog cycle, the
// spec-dispatch tick, the patrol scan, a steward scan — before it restarts
// anyway (gt-rtbbr). Each of those reruns from the new daemon: a plugin run
// records nothing until it finishes, so an interrupted one is not due-gated and
// starts again on the next heartbeat; a dog cycle's due-ness is the persisted
// last-run time; the ticks and scans run again on their own tickers. Like the
// two caps above it is measured from upgradeWaitSince rather than from the
// moment the landing work ends, so housekeeping that was already in flight when
// the marker appeared never adds a heartbeat to the wait.
//
// Two minutes is deliberately shorter than a plugin's own timeout (10m by
// default, defaultScriptTimeout): the cap exists so one long run cannot hold an
// install for minutes, and the run it cuts is not lost work — it restarts from
// the new daemon with nothing recorded, and the drain means no landing is
// competing with it.
const housekeepingRestartCap = 2 * time.Minute

// tierSweepHoldLabel is what the wait line calls a running sweep, and the value
// upgradeWaitLogged holds while the restart waits on one, so the release line
// is distinguishable from every other hold.
const tierSweepHoldLabel = "the tier sweep that is running"

// restartPendingMarker is daemon/restart-pending.json, written by
// scripts/install-gt.sh after a smoke-tested install. The daemon adds
// attempted_from (its own commit) just before it exits for the marker.
type restartPendingMarker struct {
	Commit        string `json:"commit"`
	RequestedAt   string `json:"requested_at,omitempty"`
	Source        string `json:"source,omitempty"`
	Repo          string `json:"repo,omitempty"`
	AttemptedFrom string `json:"attempted_from,omitempty"`
}

// installReceipt is one line of daemon/install-receipts.jsonl; the shell
// scripts (scripts/lib/install-gt-lib.sh igt_receipt) write the same fields.
// Absent values differ by writer: the shell writes JSON null, Go writes ""
// for strings and omits duration_s. Consumers treat null, "" and a missing
// duration_s as absent.
type installReceipt struct {
	TS         string `json:"ts"`
	Event      string `json:"event"`
	Commit     string `json:"commit"`
	PrevCommit string `json:"prev_commit"`
	Source     string `json:"source"`
	MergedAt   string `json:"merged_at"`
	Reason     string `json:"reason"`
	DurationS  int    `json:"duration_s,omitempty"`
}

func restartMarkerPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "restart-pending.json")
}

func installReceiptsPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "install-receipts.jsonl")
}

// isAncestor reports (isAncestor, known) for two commits in repo. known is
// false when git could not answer (no repo, unknown commit, git error).
func (d *Daemon) isAncestor(repo, ancestor, descendant string) (ok, known bool) {
	if repo == "" || ancestor == "" || descendant == "" {
		return false, false
	}
	ok, err := d.gitAt(repo).IsAncestor(ancestor, descendant)
	if err != nil {
		return false, false
	}
	return ok, true
}

// provenAncestor is true only when git answered and said yes. A failed or
// erroring check is never treated as ancestry.
func (d *Daemon) provenAncestor(repo, ancestor, descendant string) bool {
	ok, known := d.isAncestor(repo, ancestor, descendant)
	return known && ok
}

// provenNotAncestor is true only when git answered and said no.
func (d *Daemon) provenNotAncestor(repo, ancestor, descendant string) bool {
	ok, known := d.isAncestor(repo, ancestor, descendant)
	return known && !ok
}

// mergedAt returns the committer time of commit in repo as UTC RFC3339 (the
// shell writer's format), or "" when git cannot answer.
func (d *Daemon) mergedAt(repo, commit string) string {
	if repo == "" {
		return ""
	}
	t, err := d.gitAt(repo).CommitTime(commit)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func readRestartMarker(townRoot string) (*restartPendingMarker, error) {
	data, err := os.ReadFile(restartMarkerPath(townRoot))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m restartPendingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Commit == "" {
		return nil, fmt.Errorf("restart marker has no commit")
	}
	return &m, nil
}

// stampRestartAttempt sets attempted_from in the marker file, preserving any
// fields this daemon does not know about. It writes through a temp file and a
// rename, like every other writer of daemon/.
func stampRestartAttempt(townRoot, own string) error {
	path := restartMarkerPath(townRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	v, err := json.Marshal(own)
	if err != nil {
		return err
	}
	raw["attempted_from"] = v
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendInstallReceipt(townRoot string, r installReceipt) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(installReceiptsPath(townRoot), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// markerCovered reports whether the marker's commit is already running: equal
// to, or an ancestor of, own (merge-base --is-ancestor A A is true). Decided
// only by git ancestry in the marker's repo; when git cannot answer the marker
// is NOT covered, so the daemon restarts rather than silently clearing it.
func (d *Daemon) markerCovered(m *restartPendingMarker, own string) bool {
	return d.provenAncestor(m.Repo, m.Commit, own)
}

// restartHadNoEffect reports whether a previous restart for this marker
// (attempted_from) failed to move the daemon forward. Only a proven strict
// advance (attempted_from an ancestor of own, own not an ancestor of
// attempted_from) counts as progress; anything git cannot prove is treated as
// no effect so an unanswerable check can never loop the daemon.
func (d *Daemon) restartHadNoEffect(m *restartPendingMarker, own string) bool {
	if m.AttemptedFrom == "" {
		return false
	}
	advanced := d.provenAncestor(m.Repo, m.AttemptedFrom, own) &&
		d.provenNotAncestor(m.Repo, own, m.AttemptedFrom)
	return !advanced
}

// ownCommitForUpgrade is the daemon's own build commit (full SHA when the
// gastown checkout can resolve it), or "" when the build does not know it.
// "unknown" is treated as absent.
func (d *Daemon) ownCommitForUpgrade() string {
	raw := strings.TrimSpace(d.buildCommit())
	if raw == "" || raw == "unknown" {
		return ""
	}
	return d.resolveOwnCommit()
}

// loadMarkerForUpgrade reads the marker and the daemon's own commit. ok is
// false when there is nothing to act on.
func (d *Daemon) loadMarkerForUpgrade() (m *restartPendingMarker, own string, ok bool) {
	townRoot := d.config.TownRoot
	m, err := readRestartMarker(townRoot)
	if err != nil {
		// Writers use temp+rename, so this is corruption, not a partial write.
		d.logger.Printf("upgrade-restart: unreadable marker %s, removing: %v", restartMarkerPath(townRoot), err)
		_ = os.Remove(restartMarkerPath(townRoot))
		return nil, "", false
	}
	if m == nil {
		d.upgradeWaitCommit = ""
		return nil, "", false
	}
	own = d.ownCommitForUpgrade()
	if own == "" {
		if d.upgradeWaitCommit != m.Commit {
			d.upgradeWaitCommit = m.Commit
			d.upgradeWaitSince = time.Now()
			d.upgradeWaitEscalated = false
			d.logger.Printf("upgrade-restart: own build commit unknown; ignoring marker for %s", m.Commit)
		}
		return nil, "", false
	}
	return m, own, true
}

// clearCoveredMarker deletes a marker own already covers and appends its
// daemon_restarted receipt. Returns true when it cleared the marker.
func (d *Daemon) clearCoveredMarker(m *restartPendingMarker, own string, now time.Time) bool {
	if !d.markerCovered(m, own) {
		return false
	}
	townRoot := d.config.TownRoot
	rec := installReceipt{
		TS:         now.UTC().Format(time.RFC3339),
		Event:      "daemon_restarted",
		Commit:     m.Commit,
		PrevCommit: m.AttemptedFrom,
		Source:     m.Source,
		MergedAt:   d.mergedAt(m.Repo, m.Commit),
	}
	if t, err := time.Parse(time.RFC3339, m.RequestedAt); err == nil {
		rec.DurationS = int(now.Sub(t).Seconds())
	}
	if err := appendInstallReceipt(townRoot, rec); err != nil {
		d.logger.Printf("upgrade-restart: writing receipt: %v", err)
	}
	if err := os.Remove(restartMarkerPath(townRoot)); err != nil && !os.IsNotExist(err) {
		d.logger.Printf("upgrade-restart: removing covered marker: %v", err)
	}
	d.upgradeWaitCommit = ""
	d.logger.Printf("upgrade-restart: running %s covers marker %s; cleared", own, m.Commit)
	return true
}

// clearCoveredRestartMarker is the startup check: it clears a marker the
// running binary already covers (the daemon may have been restarted onto it
// by launchd or an operator before the marker was written). It never requests
// a restart; a newer marker is left for the heartbeat.
func (d *Daemon) clearCoveredRestartMarker(now time.Time) {
	m, own, ok := d.loadMarkerForUpgrade()
	if !ok {
		return
	}
	d.clearCoveredMarker(m, own, now)
}

// checkUpgradeRestart handles daemon/restart-pending.json at the top of every
// heartbeat. It returns true (and sets upgradeRestartRequested) when the
// daemon should shut down now so launchd restarts it on the installed binary.
// Run-loop goroutine only.
func (d *Daemon) checkUpgradeRestart(now time.Time) bool {
	m, own, ok := d.loadMarkerForUpgrade()
	if !ok {
		d.endUpgradeDrain()
		return false
	}
	if d.clearCoveredMarker(m, own, now) {
		d.endUpgradeDrain()
		return false
	}

	if d.upgradeWaitCommit != m.Commit {
		// The wait clock starts with the first pending marker and survives a
		// newer install replacing it: every install rewrites the marker, so a
		// per-commit clock never reaches postLandRestartCap (or the stuck
		// escalation) while landings keep coming.
		if d.upgradeWaitCommit == "" {
			d.upgradeWaitSince = now
		}
		d.upgradeWaitCommit = m.Commit
		d.upgradeWaitEscalated = false
	}

	if d.restartHadNoEffect(m, own) {
		d.endUpgradeDrain()
		if !d.upgradeWaitEscalated {
			d.upgradeWaitEscalated = true
			d.logger.Printf("upgrade-restart: restarted from %s for %s but now running %s; not restarting again", m.AttemptedFrom, m.Commit, own)
			d.seams.escalateUpgrade(d, "daemon:restart-pending-no-effect",
				fmt.Sprintf("Daemon restarted for upgrade to %s (from %s) but is running %s, which is not provably newer. The installed binary is probably not the marker's commit. Not restarting again.",
					m.Commit, m.AttemptedFrom, own))
		}
		return false
	}

	// Landing workers finish their pass and start no new one from here on.
	d.upgradeRestartPending.Store(true)

	// Read live, immediately before deciding: never cached across heartbeats.
	// The read is upgradeHold rather than the isIdleForUpgrade predicate it is
	// built from: the landing holds carry their own caps and are deliberately
	// not in the predicate, and the wait's own clock is what the caps read.
	if hold := d.upgradeHold(now); hold != nil {
		// One line per heartbeat naming the hold: a heartbeat that finds the
		// daemon not idle is never silent again (gt-rtbbr).
		d.logUpgradeWait(hold.name, now)
		d.noteStuckWait(now, m, own)
		return false
	}

	// The decision is taken here; the commit is exitForUpgradeIfRequested.
	// Stamping attempted_from before the commit would make a commit that
	// re-reads a landing pass and defers look like a restart that had no
	// effect, which is the one state restartHadNoEffect never retries
	// (gt-3cbee).
	d.upgradeRestartRequested.Store(true)
	return true
}

// upgradeHold reports the work that still holds a pending restart at now, or
// nil when the restart may go. A hard hold — a landing pass, a scheduled slings
// cycle, a steward job, the install lock, a maintenance gc cycle — is never
// bounded and never masked by soft work that happens to be in flight with it
// (gt-rtbbr). Work a restart can simply redo is bounded by its cap and dropped
// here; the lines naming what it cuts are written only on the heartbeat that
// actually restarts, so none of them claims a restart that then keeps waiting.
// The caps are read off upgradeWaitSince, the marker's own wait clock, so a
// hold that was already running when the marker appeared does not add a
// heartbeat to the wait.
func (d *Daemon) upgradeHold(now time.Time) *daemonWork {
	waited := now.Sub(d.upgradeWaitSince)
	if d.maintenanceGCRunning.Load() {
		// A --full gc call is in flight on the server; the daemon does not cut
		// it (maintenance_gc_guard.go). The 30m escalation still applies.
		return &daemonWork{name: "the maintenance gc cycle", hard: true}
	}
	// daemonWorkHold is hard-first, so a hard hold is returned here, unbounded,
	// even when soft work is in flight alongside it.
	hold := d.daemonWorkHold()
	if hold != nil && hold.hard {
		return hold
	}

	postLand := d.postLandRuns.Load() > 0
	// A running sweep cycle holds the restart, bounded, instead of dying with
	// the old daemon.
	sweep := d.tierSweepRunning.Load()

	switch {
	case postLand && waited < postLandRestartCap:
		return &daemonWork{name: "the post-land run"}
	case sweep && waited < tierSweepRestartCap:
		return &daemonWork{name: tierSweepHoldLabel}
	case hold != nil && waited < housekeepingRestartCap:
		return hold
	}

	// Nothing holds the restart: it goes on this heartbeat. Say what, if
	// anything, it is cutting or letting go — only now, when the restart is
	// real (gt-rtbbr).
	if postLand {
		d.logger.Printf("upgrade-restart: a post-land run is still in flight after %s; restarting anyway (the landing worker reruns the untested tip on start)", postLandRestartCap)
	}
	if sweep {
		d.logger.Printf("upgrade-restart: a tier sweep is still running after %s; restarting anyway and cutting the sweep short", tierSweepRestartCap)
	} else if d.upgradeWaitLogged == tierSweepHoldLabel {
		d.logger.Printf("upgrade-restart: the tier sweep closed; restarting")
	}
	if hold != nil {
		d.logger.Printf("upgrade-restart: %s is still in flight after %s; restarting anyway (it reruns from the new daemon)", hold.name, housekeepingRestartCap)
	}
	d.upgradeWaitLogged = ""
	return nil
}

// noteStuckWait escalates once when a pending restart has waited past
// upgradeStuckAfter for something to clear; it keeps waiting afterwards.
func (d *Daemon) noteStuckWait(now time.Time, m *restartPendingMarker, own string) {
	if d.upgradeWaitEscalated || now.Sub(d.upgradeWaitSince) < upgradeStuckAfter {
		return
	}
	d.upgradeWaitEscalated = true
	d.seams.escalateUpgrade(d, "daemon:restart-pending-stuck",
		fmt.Sprintf("Restart for upgrade to %s has waited %s for an idle daemon (running %s). Still waiting.",
			m.Commit, now.Sub(d.upgradeWaitSince).Round(time.Minute), own))
}

// endUpgradeDrain lets the landing workers start passes again: no restart is
// pending (marker gone, covered, or already tried without effect).
func (d *Daemon) endUpgradeDrain() {
	d.upgradeRestartPending.Store(false)
	d.upgradeWaitLogged = ""
	d.upgradeDrainLogged = false
}

// logUpgradeWait says what a pending restart is waiting on: the drain starting
// once per marker, then one line per heartbeat naming the hold, so a wait an
// operator is staring at always has a reason in the log (gt-rtbbr). name is
// never empty: every caller has a hold.
func (d *Daemon) logUpgradeWait(name string, now time.Time) {
	if !d.upgradeDrainLogged {
		d.upgradeDrainLogged = true
		d.logger.Printf("upgrade-restart: draining: no new landing pass until restart")
	}
	d.upgradeWaitLogged = name
	d.logger.Printf("upgrade-restart: waiting for %s (%dm)", name, int(now.Sub(d.upgradeWaitSince).Minutes()))
}

// A tier sweep cycle that started before the marker runs in this process;
// restarting kills it and its verdict is never recorded (gt-ccyw0). upgradeHold
// holds for it while it runs, up to tierSweepRestartCap. New cycles do not
// start in between: runTierSweep skips while the drain is on.

// landingPassBead is the bead of a landing pass in flight (the first rig by
// name when several), or "" when none is working on one.
func (d *Daemon) landingPassBead() string {
	best, bestRig := "", ""
	d.landingStates.each(func(rig, bead string) {
		if bestRig == "" || rig < bestRig {
			best, bestRig = bead, rig
		}
	})
	return best
}

// exitForUpgradeIfRequested is the commit of the restart checkUpgradeRestart
// requested: nil when none is requested or the daemon is no longer idle, and
// otherwise the normal shutdown (which leaves Dolt running) plus
// ErrRestartForUpgrade for Run to return, which the caller maps to exit code
// 75. Run-loop goroutine only.
func (d *Daemon) exitForUpgradeIfRequested(state *State) error {
	if !d.upgradeRestartRequested.Load() {
		return nil
	}
	m, own, ok := d.loadMarkerForUpgrade()
	if !ok {
		// The marker went away between the decision and here (or a corrupt
		// one was removed): there is nothing to restart onto, so end the
		// drain instead of holding the landing workers for a heartbeat.
		d.upgradeRestartRequested.Store(false)
		d.endUpgradeDrain()
		return nil
	}
	// The idle read behind the request raced the landing worker's own
	// registration: the worker reads the drain at the top of its loop and
	// only then counts the pass, so a pass can begin after the read saw none
	// — one merged after the queue was last seen drained as easily as any
	// other. Shutting down here would kill a gate the restart never waited
	// for, so drop the request and let the next heartbeat see the pass. The
	// drain is still on, so this pass is the last one (gt-3cbee).
	if d.landingPasses.Load() > 0 {
		d.upgradeRestartRequested.Store(false)
		d.logUpgradeWait(landingHoldName(d.landingPassBead()), time.Now())
		return nil
	}
	if err := stampRestartAttempt(d.config.TownRoot, own); err != nil {
		d.logger.Printf("upgrade-restart: could not stamp attempted_from, not restarting: %v", err)
		d.upgradeRestartRequested.Store(false)
		return nil
	}
	d.logger.Printf("upgrade-restart: idle; restarting from %s to pick up %s", own, m.Commit)
	d.logger.Println("Restarting for upgrade: shutting down so launchd restarts the daemon on the installed binary")
	_ = d.shutdown(state)
	return ErrRestartForUpgrade
}

// restartOnDrainedLanding is the run loop's response to a landing worker
// waking it at the end of a drained pass: the pass was the last thing a
// pending restart waited for, so check now and restart if idle (gt-fzwcd).
func (d *Daemon) restartOnDrainedLanding(state *State) error {
	d.checkUpgradeRestart(time.Now())
	return d.exitForUpgradeIfRequested(state)
}
