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
//
// The clock is tierSweepRestartWait's, not the marker's alone: a sweep that
// starts while the restart has already waited — one the stale bound let through
// (gt-zvqn8) — would otherwise be cut on the heartbeat after it started.
const tierSweepRestartCap = 15 * time.Minute

// tierSweepRestartWait is how long the running sweep cycle has been given: the
// smaller of the marker's wait clock and the cycle's own run, so a cycle that
// started before the marker keeps the marker's bound and a cycle that started
// after it gets that bound from its own start. A daemon with no recorded start
// — a sweep whose cycle began before this change, or a test that sets
// tierSweepRunning directly — falls back to the marker's clock.
func (d *Daemon) tierSweepRestartWait(now time.Time, waited time.Duration) time.Duration {
	started := d.tierSweepStartedAt.Load()
	if started == 0 {
		return waited
	}
	return min(max(now.Sub(time.Unix(0, started)), 0), max(waited, 0))
}

// housekeepingRestartCap is how long a pending upgrade restart waits for the
// daemon's own housekeeping — a script plugin run, a dog cycle, the
// spec-dispatch tick, the patrol scan — before it restarts
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

// maintenanceGCHoldLabel is what the wait line calls a maintenance gc cycle in
// flight (gt-oucgn).
const maintenanceGCHoldLabel = "the maintenance gc cycle"

// specDispatchHoldLabel is what the wait line calls a spec dispatch tick in
// flight (gt-cbaoy).
const specDispatchHoldLabel = "a spec dispatch tick"

// restartPendingMarker is daemon/restart-pending.json, written by the installer
// after a smoke-tested install. The daemon adds
// attempted_from (its own commit) just before it exits for the marker.
type restartPendingMarker struct {
	Commit        string `json:"commit"`
	RequestedAt   string `json:"requested_at,omitempty"`
	Source        string `json:"source,omitempty"`
	Repo          string `json:"repo,omitempty"`
	AttemptedFrom string `json:"attempted_from,omitempty"`
}

// installReceipt is one line of daemon/install-receipts.jsonl; the installer
// (internal/installer.Receipt) writes the same fields. Receipts written by the
// deleted shell installer encode an absent value as JSON null, where Go writes
// "" for strings and omits duration_s. Consumers treat null, "" and a missing
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
	return parseRestartMarker(data)
}

// parseRestartMarker parses marker JSON. A marker with no commit names no
// install, so it is an error rather than an empty marker: nothing may act on it
// (gt-oyrav).
func parseRestartMarker(data []byte) (*restartPendingMarker, error) {
	var m restartPendingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Commit == "" {
		return nil, fmt.Errorf("restart marker has no commit")
	}
	return &m, nil
}

// errRestartMarkerMoved is the stamp being dropped because the marker it was
// read from is no longer the one on disk. The daemon does not restart on it:
// the next heartbeat reads the marker that replaced it.
var errRestartMarkerMoved = errors.New("restart marker was replaced while stamping the attempt")

// stampRestartAttempt sets attempted_from in the marker file, preserving any
// fields this daemon does not know about. It writes through a temp file and a
// rename, like every other writer of daemon/.
func stampRestartAttempt(townRoot, own string) error {
	return stampRestartAttemptWith(nil, townRoot, own)
}

// stampRestartAttemptWith is stampRestartAttempt with the window between the
// temp file and the rename exposed: beforeRename, when non-nil, runs in it, so
// a test can stand in for the installer replacing the marker there.
//
// The commit is re-read immediately before the rename because that window is
// real: the installer writes the same path (its own temp file and a rename,
// under the install lock, which this writer does not take), and the daemon
// decides to restart in it. Renaming the older marker over a newer one would
// put the wrong commit back - a commit the daemon may already cover, so the
// newer install's request would be cleared and never restarted into
// (gt-oyrav).
func stampRestartAttemptWith(beforeRename func(), townRoot, own string) error {
	path := restartMarkerPath(townRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// The raw map carries the fields this daemon does not know back out
	// untouched; the typed parse is what decides which install the marker names.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	read, err := parseRestartMarker(data)
	if err != nil {
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
	// Every path below but the rename drops the temp file: nothing reads a
	// restart-pending.json.tmp, but leaving one beside the marker is sediment.
	stamped := false
	defer func() {
		if !stamped {
			_ = os.Remove(tmp)
		}
	}()

	if beforeRename != nil {
		beforeRename()
	}
	now, err := restartMarkerCommitAt(path)
	if err != nil {
		return err
	}
	if now != read.Commit {
		return fmt.Errorf("%w: %s replaced by %s", errRestartMarkerMoved, read.Commit, now)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	stamped = true
	return nil
}

// restartMarkerCommitAt reads the marker file at path and returns the commit it
// names.
func restartMarkerCommitAt(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m, err := parseRestartMarker(data)
	if err != nil {
		return "", err
	}
	return m.Commit, nil
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
// daemon_restarted receipt. Returns true when the caller must not act on m
// any further this pass: it cleared the marker, or the marker changed or could
// not be read back while git was answering and the next heartbeat reads it.
func (d *Daemon) clearCoveredMarker(m *restartPendingMarker, own string, now time.Time) bool {
	return d.clearCoveredMarkerWith(nil, m, own, now)
}

// clearCoveredMarkerWith is clearCoveredMarker with the window after the
// ancestry check exposed: beforeRemove, when non-nil, runs in it, so a test
// can stand in for the installer replacing the marker there.
//
// markerCovered runs git subprocesses while the installer writes the same
// path. The commit is re-read before the receipt and the removal, as
// stampRestartAttemptWith does before its rename: removing by path alone would
// delete a newer install's marker and leave the daemon on the old binary.
func (d *Daemon) clearCoveredMarkerWith(beforeRemove func(), m *restartPendingMarker, own string, now time.Time) bool {
	if !d.markerCovered(m, own) {
		return false
	}
	if beforeRemove != nil {
		beforeRemove()
	}
	// A marker that cannot be read back is left alone too: removing what the
	// re-read could not confirm is the delete this re-read exists to prevent.
	// The next heartbeat reads it again, and drops it if it is corrupt.
	cur, err := restartMarkerCommitAt(restartMarkerPath(d.config.TownRoot))
	if err != nil {
		d.logger.Printf("upgrade-restart: cannot re-read marker %s before clearing it; left for the next heartbeat: %v", m.Commit, err)
		return true
	}
	if cur != m.Commit {
		d.logger.Printf("upgrade-restart: marker %s replaced by %s while checking coverage; left for the next heartbeat", m.Commit, cur)
		return true
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
		d.endUpgradeDrain(now)
		return false
	}
	if d.clearCoveredMarker(m, own, now) {
		d.endUpgradeDrain(now)
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
		d.endUpgradeDrain(now)
		if !d.upgradeWaitEscalated {
			d.upgradeWaitEscalated = true
			d.logger.Printf("upgrade-restart: restarted from %s for %s but now running %s; not restarting again", m.AttemptedFrom, m.Commit, own)
			d.seams.escalateUpgrade(d, "daemon:restart-pending-no-effect",
				fmt.Sprintf("Daemon restarted for upgrade to %s (from %s) but is running %s, which is not provably newer. The installed binary is probably not the marker's commit. Not restarting again.",
					m.Commit, m.AttemptedFrom, own))
		}
		return false
	}

	// Read live, immediately before deciding: never cached across heartbeats.
	// The read is upgradeHold rather than the isIdleForUpgrade predicate it is
	// built from: the landing holds carry their own caps and are deliberately
	// not in the predicate, and the wait's own clock is what the caps read.
	work := d.daemonWorkHold()
	hold := d.upgradeHold(now, work)
	if d.upgradeDrainDeferred(hold) {
		// A tier sweep or dispatch tick that began before the marker is the
		// hold: landing passes keep starting until it closes or hits its bound
		// (gt-du6xl, gt-u1fzb).
		d.deferUpgradeDrain(now, hold.name)
		d.noteStuckWait(now, m, own)
		return false
	}

	// Landing workers finish their pass and start no new one from here on.
	if !d.upgradeRestartDrains() {
		// The sling wait is frozen here, where the drain turns on: no sling
		// starts after this, so the set only shrinks and its youngest member
		// gets the bound it had when the drain began (gt-nosod).
		d.upgradeSlingSince, _ = d.specSlings.newest()
		d.upgradeSlingFrozen = true
	}
	d.landingHolds.hold(landingHoldUpgradeRestart)
	// The drain is on from here, and that is what parks the ticker: the tick
	// wait belonged to the deferred wait above, so it ends with it. It is
	// cleared after the drain is set, so no ticker pass sees a restart that
	// parks nothing (gt-u1fzb).
	d.endDispatchTickWait()

	if hold != nil {
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
// nil when the restart may go. A hard hold — a landing pass, a scheduled
// slings cycle, the install lock — is never
// bounded and never masked by soft work that happens to be in flight with it
// (gt-rtbbr). Work a restart can simply redo is bounded by its cap and dropped
// here; the lines naming what it cuts are written only on the heartbeat that
// actually restarts, so none of them claims a restart that then keeps waiting.
// The caps are read off upgradeWaitSince, the marker's own wait clock, so a
// hold that was already running when the marker appeared does not add a
// heartbeat to the wait.
//
// work is the caller's one daemonWorkHold read for this decision, so the hold
// returned here and the drain decision made from it see the same daemon state.
//
// A maintenance gc cycle holds a restart like hard work, but bounded by the
// gc's own clock rather than the marker's (maintenanceGCRestartCap): its
// context comes from the daemon's, so a restart inside the cycle aborts the
// call, and a stuck gc must not hold an install forever (gt-oucgn).
func (d *Daemon) upgradeHold(now time.Time, work *daemonWork) *daemonWork {
	waited := now.Sub(d.upgradeWaitSince)

	// daemonWorkHold is hard-first, so a hard hold is returned here, unbounded,
	// even when soft work is in flight alongside it. The gc's clock is told
	// whether it is, so it does not run while a hard hold is what defers the
	// restart.
	hold := work
	hardHeld := hold != nil && hold.hard
	gcWaited, gcRunning := d.maintenanceGCUpgradeHold(now, hardHeld)
	if hardHeld {
		return hold
	}

	// A gc cycle is in flight and the daemon does not cut its call
	// (maintenance_gc_guard.go), so the restart waits for the cycle to close —
	// up to the gc call's own bound, past which the call is stuck (gt-oucgn).
	// The wait is read off the call's own clock, not the marker's or the
	// cycle's, so a gc that starts late in a wait, or after a long backup,
	// still gets its full bound.
	if gcRunning && gcWaited < maintenanceGCRestartCap {
		return &daemonWork{name: maintenanceGCHoldLabel, hard: true}
	}

	postLand := d.postLandRuns.Load() > 0
	// A running sweep cycle holds the restart, bounded, instead of dying with
	// the old daemon.
	sweep := d.tierSweepRunning.Load()
	sweepWaited := d.tierSweepRestartWait(now, waited)
	// A spec dispatch tick holds the restart up to its own bound, not the
	// housekeeping cap: the tick is cut at that bound whatever the restart
	// does, and a restart that cuts it mid-sling leaves the seat hooked with no
	// session (gt-cbaoy).
	spec := d.specDispatchRunning.Load()
	specWaited := d.specDispatchRestartWait(now, waited)
	var specBound time.Duration
	if spec {
		specBound = d.upgradeHoldBound(specDispatchHoldLabel)
	}

	// The slings a tick handed back to run in the background hold the restart
	// the same way, each up to the sling's own bound. The wait runs from the
	// youngest start the drain froze, not the oldest's: from the oldest's it
	// ended when that sling was cut while a younger one still had most of its
	// own left (gt-nosod). It does not run from a sling that started after the
	// drain, which would let new slings extend it for ever (gt-5h70s).
	slingSince, slinging := d.specSlings.newest()
	if d.upgradeSlingFrozen {
		// The drain froze the slings in flight: those hold the restart from their
		// youngest start, and a sling that started after the drain holds nothing,
		// so new slings cannot extend the wait (gt-nosod). A drain that froze none
		// holds on none.
		slingSince = d.upgradeSlingSince
		slinging = slinging && !d.upgradeSlingSince.IsZero()
	}
	slingWaited := max(now.Sub(slingSince), 0)

	switch {
	case postLand && waited < postLandRestartCap:
		return &daemonWork{name: "the post-land run"}
	case sweep && sweepWaited < tierSweepRestartCap:
		return &daemonWork{name: tierSweepHoldLabel}
	case spec && specWaited < specBound:
		return &daemonWork{name: specDispatchHoldLabel}
	case slinging && slingWaited < specSlingBound():
		return &daemonWork{name: specSlingHoldLabel}
	case hold != nil && !hold.ownBound && waited < housekeepingRestartCap:
		// The slings have their own case above: past their bound the housekeeping
		// cap does not hold them longer.
		return hold
	}

	// Nothing holds the restart: it goes on this heartbeat. Say what, if
	// anything, it is cutting or letting go — only now, when the restart is
	// real (gt-rtbbr).
	if gcRunning {
		d.logger.Printf("upgrade-restart: the maintenance gc call has run for %s, past its %s bound; restarting anyway and the shutdown cancels it",
			gcWaited.Round(time.Second), maintenanceGCRestartCap)
	}
	if postLand {
		d.logger.Printf("upgrade-restart: a post-land run is still in flight after %s; restarting anyway (the landing worker reruns the untested tip on start)", postLandRestartCap)
	}
	if sweep {
		d.logger.Printf("upgrade-restart: a tier sweep has run for %s without closing; restarting anyway and cutting the sweep short",
			sweepWaited.Round(time.Second))
	} else if d.upgradeWaitLogged == tierSweepHoldLabel {
		d.logger.Printf("upgrade-restart: the tier sweep closed; restarting")
	}
	if spec && specWaited >= specBound {
		d.logger.Printf("upgrade-restart: %s is still in flight after its %s bound; restarting anyway (it reruns from the new daemon)", specDispatchHoldLabel, specBound)
	}
	if slinging && slingWaited >= specSlingBound() {
		d.logger.Printf("upgrade-restart: %s is still in flight after its %s bound; restarting anyway (the bead is read again by the new daemon's first tick)", specSlingHoldLabel, specSlingBound())
	}
	if hold != nil && hold.name != specDispatchHoldLabel && !hold.ownBound {
		d.logger.Printf("upgrade-restart: %s is still in flight after %s; restarting anyway (it reruns from the new daemon)", hold.name, housekeepingRestartCap)
	}
	d.upgradeWaitLogged = ""
	return nil
}

// specDispatchRestartWait is how long the running spec dispatch tick has been
// given: its own run since triggerSpecDispatch recorded its start, because the
// tick is cut at its bound from that start. A tick with no recorded start (a
// test that sets specDispatchRunning directly) falls back to the marker's
// clock.
func (d *Daemon) specDispatchRestartWait(now time.Time, waited time.Duration) time.Duration {
	run := d.specDispatchTick.Load()
	if run == nil {
		return waited
	}
	return max(now.Sub(run.started), 0)
}

// specDispatchRunBound is the bound the running tick was triggered under, so
// the restart's wait and the tick's own cut agree even when the config changed
// or stopped reading mid-tick (gt-cbaoy). A tick that recorded no bound (a test
// that sets the flag and start directly) reads the config's, as before.
func (d *Daemon) specDispatchRunBound() time.Duration {
	if run := d.specDispatchTick.Load(); run != nil && run.bound > 0 {
		return run.bound
	}
	return d.specDispatchTickBound()
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

// upgradeDrainDeferred reports whether the wait for hold runs without the
// drain: hold is the running tier sweep or the spec dispatch tick in flight, and
// it began before the marker. One that began after the marker keeps the drain,
// as it always has. The restart waits on either hold for its own bound either
// way: tierSweepRestartCap for a sweep, the tick's run bound for a tick
// (gt-du6xl, gt-cbaoy).
//
// Soft daemon work in flight does not keep the drain on: upgradeHold names the
// sweep and the tick ahead of it, and a plugin run, a scan or a paid pass has no
// landing pass to protect (gt-9vjw8). A dispatch tick has none either: it is
// soft work whose slings run on after it, so the drain would hold landings for
// the tick's whole bound to protect nothing (gt-u1fzb). Hard work still keeps
// the drain, and upgradeHold returns any hard hold before the sweep and the
// tick, so a hold that names one is already the hard-free case.
func (d *Daemon) upgradeDrainDeferred(hold *daemonWork) bool {
	if hold == nil {
		return false
	}
	switch hold.name {
	case tierSweepHoldLabel:
		// A cycle with no recorded start runs on the marker's clock, as in
		// tierSweepRestartWait, so it counts as one from before the marker.
		started := d.tierSweepStartedAt.Load()
		return started == 0 || time.Unix(0, started).Before(d.upgradeWaitSince)
	case specDispatchHoldLabel:
		// A tick with no recorded start counts as one from before the marker, as
		// a sweep with none does.
		run := d.specDispatchTick.Load()
		return run == nil || run.started.Before(d.upgradeWaitSince)
	}
	return false
}

// deferUpgradeDrain is the wait for the hold upgradeHold named ahead of any soft
// work in flight, whose label and bound it takes. Landing workers are not
// drained, so passes keep starting while the hold runs. The wait line still
// names the hold on every heartbeat, and the deferral is logged once per hold
// (gt-du6xl, gt-u1fzb). A drain that was already on — a landing pass or other
// hold had it going — ends here, and its end line says for how long it held
// landings (gt-9vjw8).
func (d *Daemon) deferUpgradeDrain(now time.Time, name string) {
	// The ticker is parked by the restart's landing hold while the drain is on,
	// and this call is what clears it. While the hold is the tick in flight, the
	// tick wait is set before the clear, so no ticker pass sees a restart that
	// is waiting park nothing: a tick started in the gap would outlast the wait
	// and be cut by the restart, leaving a sling half done (gt-cbaoy, gt-u1fzb).
	d.upgradeTickWaiting.Store(name == specDispatchHoldLabel)
	d.resumeDispatchAfterDrain()
	d.reportDrainHeld(now)
	if d.upgradeWaitLogged != name {
		d.logger.Printf("upgrade-restart: landing passes continue while %s holds the restart; the drain starts when it closes or after its %s bound",
			name, d.upgradeHoldBound(name))
	}
	d.upgradeWaitLogged = name
	d.logger.Printf("upgrade-restart: waiting for %s (%dm)", name, int(now.Sub(d.upgradeWaitSince).Minutes()))
}

// upgradeHoldBound is how long the restart gives a hold with its own bound. The
// wait and the deferral line read it the same way, so the line names the bound
// the restart enforces (gt-u1fzb).
func (d *Daemon) upgradeHoldBound(name string) time.Duration {
	if name == specDispatchHoldLabel {
		return d.specDispatchRunBound()
	}
	return tierSweepRestartCap
}

// endUpgradeDrain lets the landing workers start passes again: no restart is
// pending (marker gone, covered, or already tried without effect). A drain that
// was on ends here and reports how long it held landings (gt-9vjw8).
func (d *Daemon) endUpgradeDrain(now time.Time) {
	d.resumeDispatchAfterDrain()
	// A wait on the dispatch tick in flight ends with the drain: a tick the
	// restart no longer waits on does not keep the ticker parked (gt-u1fzb).
	d.endDispatchTickWait()
	d.reportDrainHeld(now)
	d.upgradeWaitLogged = ""
	d.resetUpgradeGCHold()
}

// resumeDispatchAfterDrain lets spec slings start again: no restart is pending,
// and the sling wait is unfrozen so the next drain measures its own youngest
// start (gt-nosod). It leaves the tick wait alone: deferUpgradeDrain sets it
// before this call while the restart still waits on a tick, and the paths that
// end the wait clear it with endDispatchTickWait.
func (d *Daemon) resumeDispatchAfterDrain() {
	d.landingHolds.release(landingHoldUpgradeRestart)
	d.upgradeSlingSince = time.Time{}
	d.upgradeSlingFrozen = false
}

// endDispatchTickWait clears the notice that the restart waits on the spec
// dispatch tick in flight. Every path that ends that wait clears it — the drain
// taking over, and the wait ending with the marker — so the ticker is never left
// parked by a wait nobody is in (gt-u1fzb). Clearing it never unparks a drain:
// the drain's own flag is what parks the ticker there.
func (d *Daemon) endDispatchTickWait() {
	d.upgradeTickWaiting.Store(false)
}

// reportDrainHeld writes the one line a drain leaves behind when it ends: how
// long it held landings and the last hold its wait lines named, so an operator
// reading the log alone can tell what the delay cost and what it waited for
// (gt-9vjw8). No drain, no line; the bookkeeping is cleared either way. The
// hold is upgradeDrainWait rather than upgradeWaitLogged, which upgradeHold
// clears on the heartbeat that goes ahead with the restart.
func (d *Daemon) reportDrainHeld(now time.Time) {
	if d.upgradeDrainLogged {
		d.logger.Printf("upgrade-restart: the drain held landing passes for %s, last waiting for %s",
			now.Sub(d.upgradeDrainSince).Round(time.Second), d.upgradeDrainWait)
	}
	d.upgradeDrainLogged = false
	d.upgradeDrainSince = time.Time{}
	d.upgradeDrainWait = ""
}

// logUpgradeWait says what a pending restart is waiting on: the drain starting
// once per marker, then one line per heartbeat naming the hold, so a wait an
// operator is staring at always has a reason in the log (gt-rtbbr). name is
// never empty: every caller has a hold.
func (d *Daemon) logUpgradeWait(name string, now time.Time) {
	if !d.upgradeDrainLogged {
		d.upgradeDrainLogged = true
		d.upgradeDrainSince = now
		d.logger.Printf("upgrade-restart: draining: no new landing pass until restart")
	}
	d.upgradeWaitLogged = name
	d.upgradeDrainWait = name
	d.logger.Printf("upgrade-restart: waiting for %s (%dm)", name, int(now.Sub(d.upgradeWaitSince).Minutes()))
}

// A tier sweep cycle that started before the marker runs in this process;
// restarting kills it and its verdict is never recorded (gt-ccyw0). upgradeHold
// holds for it while it runs, up to tierSweepRestartCap. New cycles do not
// start in between unless the last sweep is already past the stale bound
// (tierSweepDeferForRestart, gt-zvqn8), which is the one case where the
// restart waits on a sweep that began after the marker did. While a sweep that
// began before the marker is what the restart waits on, upgradeDrainDeferred
// keeps the landing drain off (gt-du6xl, gt-9vjw8).
//
// A spec dispatch tick that started before the marker is the same case: its
// slings run on after it and it protects no landing pass, so the drain waits
// with it and upgradeTickWaiting keeps a new tick from starting behind it
// (gt-u1fzb). A tick that began after the marker holds the drain on, as a late
// sweep does.

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
// 75. now is the clock of the decision that requested the restart, so the
// durations this commit writes are measured on the marker's wait clock rather
// than mixing in a second clock. Run-loop goroutine only.
func (d *Daemon) exitForUpgradeIfRequested(state *State, now time.Time) error {
	if !d.upgradeRestartRequested.Load() {
		return nil
	}
	m, own, ok := d.loadMarkerForUpgrade()
	if !ok {
		// The marker went away between the decision and here (or a corrupt
		// one was removed): there is nothing to restart onto, so end the
		// drain instead of holding the landing workers for a heartbeat.
		d.upgradeRestartRequested.Store(false)
		d.endUpgradeDrain(now)
		return nil
	}
	// The idle read can miss a landing pass that is starting: the worker
	// reads the drain, then counts itself. Shutting down now would kill a
	// gate the restart never waited for, so drop the request and let the
	// next heartbeat see the pass. The drain stays on, so that pass is the
	// last one (gt-3cbee).
	if d.landingPasses.Load() > 0 {
		d.upgradeRestartRequested.Store(false)
		d.logUpgradeWait(landingHoldName(d.landingPassBead()), now)
		return nil
	}
	if err := stampRestartAttempt(d.config.TownRoot, own); err != nil {
		d.logger.Printf("upgrade-restart: could not stamp attempted_from, not restarting: %v", err)
		// The pending flag goes with the request. A restart that did not stamp
		// is not waited on, so spec dispatch and landing passes run until the
		// next heartbeat decides again; keeping the flag would hold them for as
		// long as the stamp keeps failing (gt-21cr0).
		d.upgradeRestartRequested.Store(false)
		d.resumeDispatchAfterDrain()
		d.endDispatchTickWait()
		return nil
	}
	// A drain that is on ends with this restart: name the time it cost before
	// the restart lines, so the log of an upgrade says how long landings were
	// held and on what (gt-9vjw8).
	d.reportDrainHeld(now)
	d.logger.Printf("upgrade-restart: idle; restarting from %s to pick up %s", own, m.Commit)
	d.logger.Println("Restarting for upgrade: shutting down so launchd restarts the daemon on the installed binary")
	_ = d.shutdown(state)
	// Armed after the shutdown so the helper's wait starts once the old
	// daemon is already on its way out. If launchd cannot respawn it, the
	// helper heals the job (gt-eigyu.1).
	d.armUpgradeHeal()
	return ErrRestartForUpgrade
}

// restartOnDrainedLanding is the run loop's response to a landing worker
// waking it at the end of a drained pass: the pass was the last thing a
// pending restart waited for, so check now and restart if idle (gt-fzwcd).
// One clock serves both the check and the commit.
func (d *Daemon) restartOnDrainedLanding(state *State) error {
	now := time.Now()
	d.checkUpgradeRestart(now)
	return d.exitForUpgradeIfRequested(state, now)
}
