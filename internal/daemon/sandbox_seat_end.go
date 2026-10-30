package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/jev/battery"
	jevrules "github.com/steveyegge/gastown/internal/jev/rules"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// A sandbox seat is single-use. Its work lives in the sandbox: the sling's host
// worktree is a snapshot the seat's commits never reach, and once the sandbox
// is gone and the work has landed the only host reader left is the rules
// battery, which asks about the session by reading its transcript. The step
// below removes the worktree, so finished seats stop leaving a polecat
// directory apiece behind, and waits for that last ask (gt-sefzt, gt-381oj). A
// tmux seat keeps its worktree and is never touched here.

// sandboxSeatLanding is the landing that finished a sandbox seat: the bead, the
// branch its work was on, when the seat submitted it, and whether the rig's
// landing record proves that work landed. A landing the record does not prove
// is unlanded work, which keeps the seat.
type sandboxSeatLanding struct {
	BeadID string
	Branch string
	// Submitted is when the seat submitted the bead for landing: the landing
	// worker already holds it, and it is the stamp the rules battery's cursor
	// is compared against (gt-381oj). Zero when the bead carries no
	// submission stamp.
	Submitted time.Time
	Landed    bool
}

// sandboxSeatEnd is what the daemon measured about a seat before deciding
// whether its host directories may go. Every field is a fact; keepReason is the
// one place the facts become the decision.
type sandboxSeatEnd struct {
	// SandboxSeat is whether the seat's agent is an openshell seat in the
	// town's settings.
	SandboxSeat bool
	// SandboxGone is the gateway's answer that it has no sandbox of the
	// seat's name.
	SandboxGone bool
	// Landed is the landing record's proof that the seat's work is on the
	// target.
	Landed bool
	// Retired is whether the seat's intent record says stop and carries no
	// operator hold: nothing wants the seat again until work is dispatched to
	// it, and the operator did not park or freeze it.
	Retired bool
	// HookBead is the work the agent bead still hooks, empty after gt done.
	HookBead string
	// Branch is the branch the agent bead records for the seat, and
	// LandedBranch the branch its work was landed from.
	Branch       string
	LandedBranch string
	// CleanupStatus is the git state gt done recorded on the agent bead.
	CleanupStatus polecat.CleanupStatus
	// PushFailed is the push failure gt done recorded.
	PushFailed bool
	// RulesPending is whether the jev_rules patrol may still ask about the
	// seat's last submission: the battery reads the transcript out of the
	// host worktree this step removes (gt-381oj).
	RulesPending bool
}

// rulesPendingReason is the keep reason of a seat whose only outstanding fact
// is the rules battery's ask: every other keep reason names a fact that stays
// true, and this one is answered with time, so the end offers the seat again
// on later passes until it is (gt-381oj).
const rulesPendingReason = "jev_rules has not yet asked about this submission"

// keepReason names why the seat's host directories must stay, or "" when they
// may be removed. Every reason is a fact someone else measured; the first one
// this file can answer decides. A missing fact keeps the seat: the removal
// destroys the only host copy of whatever the seat did, so it waits for an
// answer it can trust (gt-sefzt).
func (e sandboxSeatEnd) keepReason() string {
	switch {
	case !e.SandboxSeat:
		return "not a sandbox seat: a tmux seat keeps its worktree"
	case !e.SandboxGone:
		return "the gateway has not confirmed the sandbox gone"
	case !e.Landed:
		return "no landing record proves its work landed (unlanded)"
	case !e.CleanupStatus.IsSafe():
		return "cleanup_status=" + orMissing(e.CleanupStatus) + ": unpushed or unrecorded work keeps the seat"
	case e.PushFailed:
		return "push_failed=true: the seat's branch never reached origin"
	case e.RulesPending:
		return rulesPendingReason
	case strings.TrimSpace(e.HookBead) != "":
		return "hook_bead=" + e.HookBead + " is still set"
	case strings.TrimSpace(e.Branch) == "" || strings.TrimSpace(e.LandedBranch) == "":
		return "branch=" + orMissingText(e.Branch) + " landed_branch=" + orMissingText(e.LandedBranch) + ": a branch nothing recorded cannot show this seat's work is the landed work"
	case e.Branch != e.LandedBranch:
		return "the seat works " + e.Branch + ", not the landed " + e.LandedBranch
	case !e.Retired:
		return "the seat's record does not say stop: something still wants it"
	}
	return ""
}

// orMissingText names a branch nothing recorded, as orMissing does a status.
func orMissingText(text string) string {
	if strings.TrimSpace(text) == "" {
		return "<missing>"
	}
	return text
}

// orMissing names a cleanup status nothing recorded, so a keep reason never
// ends after the colon with an empty value.
func orMissing(status polecat.CleanupStatus) string {
	if strings.TrimSpace(string(status)) == "" {
		return "<missing>"
	}
	return string(status)
}

// endFinishedSandboxSeat removes the host directories the sandbox seat rig/name
// left once its landing is recorded and every guard in sandboxSeatEnd agrees
// nothing of its work is at risk. It reports whether the directories were
// removed; a kept seat and a failed removal are both logged with their reason.
//
// Every fact but the landing record (read once by the caller, before the lock)
// is measured under the polecat's directory lock, the one a sling
// takes before it builds the worktree, and the removal runs under the same
// hold. A sling that starts while the seat is ending either waits for the
// removal and then builds afresh, or has already written the facts that keep
// the seat. The step is idempotent, so a seat whose directories are already
// gone costs one stat and nothing else.
func (h *patrolScanHost) endFinishedSandboxSeat(rig, name string, landing sandboxSeatLanding) bool {
	log := func(format string, args ...any) {
		h.d.logger.Printf("sandbox seat end %s/%s: "+format, append([]any{rig, name}, args...)...)
	}
	if err := polecat.CheckSandboxSeatNames(rig, name); err != nil {
		log("kept: %v", err)
		return false
	}
	// A cheap read outside the lock: a polecat that is not an openshell seat
	// takes no lock and asks nothing.
	fields, err := h.seatAgentFields(rig, name)
	if err != nil {
		log("kept: cannot read its agent bead: %v", err)
		return false
	}
	if fields == nil {
		log("kept: no agent bead, so this town does not track it as a seat")
		h.clearPendingSeatEnd(rig, name)
		return false
	}
	if _, ok, err := h.openshellSeatFor(fields.Agent); err != nil {
		log("kept: %v", err)
		return false
	} else if !ok {
		// A tmux seat keeps today's behaviour, silently. Whatever a later
		// pass recorded about the seat is answered: it is not a sandbox seat.
		h.clearPendingSeatEnd(rig, name)
		return false
	}
	rigPath := filepath.Join(h.town(), rig)
	var (
		kept       string
		unmeasured string
		partial    string
		removed    bool
	)
	err = polecat.WithSandboxSeatLock(rigPath, name, func() error {
		end, unmeasuredReason := h.measureSandboxSeatEnd(rig, name, landing)
		if unmeasuredReason != "" {
			unmeasured = unmeasuredReason
			return nil
		}
		if why := end.keepReason(); why != "" {
			kept = why
			return nil
		}
		deleted, err := h.removeSeatDirs(rigPath, rig, name)
		if err != nil {
			// A removal that deleted something and then failed has not kept
			// the seat: saying so would send an operator looking for a seat
			// that is half gone (gt-sefzt).
			if deleted {
				partial = err.Error()
			} else {
				kept = "removing its host directories: " + err.Error()
			}
			return nil
		}
		removed = true
		return nil
	})
	switch {
	case err != nil:
		// The lock itself failed, so nothing was measured: whatever a later
		// pass recorded about this seat is left for that pass to retry.
		log("kept: %v", err)
		return false
	case partial != "":
		// The seat is half gone; nothing is left to wait for the rules ask.
		h.clearPendingSeatEnd(rig, name)
		log("its host directories were partly removed, then the removal failed: %s (the seat is finished: landed %s)", partial, landing.BeadID)
		return false
	case unmeasured != "":
		// A fact that could not be read is the fact a later pass retries, so
		// the record, if there is one, stays.
		log("kept: %s", unmeasured)
		return false
	case kept == rulesPendingReason:
		// The landing's own run of this step is spent, so the seat is recorded
		// with its landing facts and offered again on later passes until the
		// ask has happened or the window has closed (gt-381oj).
		h.recordPendingSeatEnd(rig, name, landing)
		log("kept: %s", kept)
		return false
	case kept != "":
		// A fact that stays true decides the seat: no pass waits for it.
		h.clearPendingSeatEnd(rig, name)
		log("kept: %s", kept)
		return false
	case removed:
		h.clearPendingSeatEnd(rig, name)
		log("removed its host directories: the seat is finished (landed %s)", landing.BeadID)
	}
	return removed
}

// recordPendingSeatEnd leaves the seat's landing facts for a later pass, which
// offers the same end again. The stamp is clamped to now, so a seat whose
// submission the landing worker stamped in the future still waits only
// rulesPendingWindow from here (gt-8yk78, gt-381oj). A write that fails is
// logged: the seat stays, and nothing but a later landing on the same polecat
// will offer it again.
func (h *patrolScanHost) recordPendingSeatEnd(rig, name string, landing sandboxSeatLanding) {
	landing.Submitted = rulesAskStamp(h.d.clk().Now(), landing.Submitted)
	rec := pendingSandboxSeatEnd{Landing: landing}
	if err := writePendingSandboxSeatEnd(h.town(), rig, name, rec); err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot record the seat for a later rules pass: %v", rig, name, err)
	}
}

// clearPendingSeatEnd forgets a record this end has just decided on.
func (h *patrolScanHost) clearPendingSeatEnd(rig, name string) {
	if err := removePendingSandboxSeatEnd(h.town(), rig, name); err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot remove its pending record: %v", rig, name, err)
	}
}

// measureSandboxSeatEnd reads the facts the seat's end decides on. It runs
// under the seat's lock, so each fact is the one a sling that holds the lock
// would see. A fact that cannot be read comes back as the reason the seat is
// kept, and nothing is decided from a partial read.
func (h *patrolScanHost) measureSandboxSeatEnd(rig, name string, landing sandboxSeatLanding) (sandboxSeatEnd, string) {
	fields, err := h.seatAgentFields(rig, name)
	if err != nil {
		return sandboxSeatEnd{}, "cannot read its agent bead: " + err.Error()
	}
	if fields == nil {
		return sandboxSeatEnd{}, "its agent bead is gone"
	}
	seat, sandboxed, err := h.openshellSeatFor(fields.Agent)
	if err != nil {
		return sandboxSeatEnd{}, err.Error()
	}
	end := sandboxSeatEnd{
		SandboxSeat:   sandboxed,
		Landed:        landing.Landed,
		HookBead:      fields.HookBead,
		Branch:        strings.TrimSpace(fields.Branch),
		LandedBranch:  strings.TrimSpace(landing.Branch),
		CleanupStatus: polecat.CleanupStatus(fields.CleanupStatus),
		PushFailed:    fields.PushFailed,
	}
	if sandboxed {
		exists, err := h.sandboxStillExists(seat.gateway, h.sessionName(rig, name))
		if err != nil {
			return sandboxSeatEnd{}, fmt.Sprintf("cannot ask gateway %q whether the sandbox is gone: %v", seat.gateway, err)
		}
		end.SandboxGone = !exists
	}
	rec, err := intent.Read(h.town(), supervisor.IntentSeat(h.seat(rig, name)))
	if err != nil {
		return sandboxSeatEnd{}, "cannot read its intent record: " + err.Error()
	}
	end.Retired = !rec.Held() && rec.EffectiveDesired() == intent.DesiredStop
	end.RulesPending = h.rulesPending(rig, name, landing.Submitted)
	return end, ""
}

// rulesPendingWindow is how long after its submission a seat waits for the
// rules battery to ask about it. Past it the seat goes whatever the cursor
// says, so a stuck rules patrol cannot leave directories behind for ever
// (gt-381oj).
const rulesPendingWindow = time.Hour

// rulesAskStamp is the stamp the rules wait counts from: a seat's clock may run
// ahead of the daemon's, so a stamp in the future is clamped to now. That is the
// rule closeSubmittedSpan applies to a landing's stamp (gt-8yk78), and it is
// what stops a future stamp keeping the seat past rulesPendingWindow from now
// (gt-381oj).
func rulesAskStamp(now, submitted time.Time) time.Time {
	if submitted.After(now) {
		return now
	}
	return submitted
}

// rulesPending reports whether the jev_rules patrol may still ask about a
// submission stamped submitted: the patrol is on, the stamp is inside
// rulesPendingWindow, and the battery's cursor has not reached it. The battery
// reads the transcript out of the host worktree, so a submission it has not
// asked about yet is the only thing that worktree is still wanted for
// (gt-381oj).
//
// A submission with no stamp is not pending: the seat is kept only on a fact
// this step measured, never on the absence of one.
//
// A cursor this town cannot read is not pending either, and it says so once
// with a line naming the seat, so an unreadable cursor never looks like a
// clean one. A cursor not written yet is not pending and says nothing: the
// battery has asked about nothing, and there is no fact to report (gt-381oj).
func (h *patrolScanHost) rulesPending(rig, name string, submitted time.Time) bool {
	if submitted.IsZero() || !h.d.isPatrolActive("jev_rules") {
		return false
	}
	now := h.d.clk().Now()
	submitted = rulesAskStamp(now, submitted)
	if now.Sub(submitted) >= rulesPendingWindow {
		return false
	}
	at, ok, err := h.d.readRulesCursor()
	if err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: jev_rules cursor unreadable: %v; the seat is not held for rules", rig, name, err)
		return false
	}
	if !ok {
		return false
	}
	return at.Before(submitted)
}

// readRulesCursor reads the stamp of the jev_rules battery's cursor, through
// the daemon's seam when it has one, and reports whether the town has written
// one.
func (d *Daemon) readRulesCursor() (time.Time, bool, error) {
	path := jevrules.CursorPath(d.config.TownRoot)
	if d.rulesCursorAt != nil {
		return d.rulesCursorAt(path)
	}
	cur, ok, err := battery.ReadCursor(path)
	if err != nil || !ok {
		return time.Time{}, ok, err
	}
	return cur.Time, true, nil
}

// seatAgentFields reads the polecat's agent bead. A bead that does not exist is
// nil with no error; a read that failed is an error, never "no bead".
func (h *patrolScanHost) seatAgentFields(rig, name string) (*beads.AgentFields, error) {
	id := beads.AgentBeadIDFor(constants.RolePolecat, rig, name, h.town())
	if id == "" {
		return nil, nil
	}
	_, fields, err := beads.GetAgentBead(h.recoveryBeads(), id)
	if err != nil {
		return nil, err
	}
	return fields, nil
}

// removeSeatDirs removes the host directories of one sandbox seat, through the
// implementation the host was given: nil is polecat.RemoveSandboxSeatHostDirs,
// and a test injects one that starts no git. The bool reports whether anything
// was deleted, so the caller can log a removal that failed halfway as such.
func (h *patrolScanHost) removeSeatDirs(rigPath, rig, name string) (bool, error) {
	if h.removeSandboxSeatDirs != nil {
		return h.removeSandboxSeatDirs(rigPath, rig, name)
	}
	return polecat.RemoveSandboxSeatHostDirs(rigPath, rig, name, nil)
}

// sandboxSeatEndJoinBudget bounds how long stopSandboxSeatEnds waits for the
// seat ends in flight. The gateway question a seat end asks is canceled by
// shutdown, so the wait is short in practice; the bound is what keeps
// shutdown's ceiling a fact when one end is wedged.
const sandboxSeatEndJoinBudget = 2 * time.Second

// startSandboxSeatEnd is the landing worker's after-landing hook: it runs the
// seat's end off the landing's goroutine, since the gateway question it asks is
// bounded but slow, and a failed cleanup must never take the landing loop down.
// A daemon that is already stopping starts no end: the seat keeps its
// directories, which is the safe answer.
//
// The stop check and the count are one step under sandboxSeatEndMu, the lock
// stopSandboxSeatEnds takes before it waits: an end added after that wait
// started would be one the shutdown never joins, and the two would race.
func (d *Daemon) startSandboxSeatEnd(rigName string, landings *land.LandingsFile, w land.Work) {
	name := strings.TrimSpace(w.Worker)
	if name == "" {
		return // a crew landing has no seat
	}
	d.startSandboxSeatEndStep(rigName, name, func() {
		d.runSandboxSeatEnd(rigName, landings, w)
	})
}

// startSandboxSeatEndStep runs one seat end off the caller's goroutine, with
// the shutdown accounting every seat end shares: a daemon that is already
// stopping starts none, and stopSandboxSeatEnds joins the ones in flight.
func (d *Daemon) startSandboxSeatEndStep(rigName, name string, run func()) {
	d.sandboxSeatEndMu.Lock()
	if d.sandboxSeatEndStopped || d.ctxOrBackground().Err() != nil {
		d.sandboxSeatEndMu.Unlock()
		d.logger.Printf("sandbox seat end %s/%s: kept: the daemon is stopping", rigName, name)
		return
	}
	d.sandboxSeatEndCycles.Add(1)
	d.sandboxSeatEndMu.Unlock()
	go func() {
		defer d.sandboxSeatEndCycles.Done()
		defer func() {
			if r := recover(); r != nil {
				d.logger.Printf("landing_worker: %s: sandbox seat end panicked: %v", rigName, r)
			}
		}()
		run()
	}()
}

// retryPendingSandboxSeatEnds offers every recorded seat to the end again. The
// end measures the rules ask afresh, so a seat whose ask has happened, or whose
// window has closed, is removed here; one still waiting is kept and recorded
// again for the next pass. Without this a seat kept for the ask would wait for
// its polecat name to be slung again, and the transcript the battery reads
// would sit there unread (gt-381oj).
func (d *Daemon) retryPendingSandboxSeatEnds() {
	if d.config == nil {
		return
	}
	records, bad, err := readPendingSandboxSeatEnds(d.config.TownRoot)
	for _, path := range bad {
		// An unreadable record is reported, never silently skipped: its seat
		// is not offered again until someone can read the record.
		d.logger.Printf("sandbox seat end: cannot read the record %s, so its seat is not offered again", path)
	}
	if err != nil {
		d.logger.Printf("sandbox seat end: cannot list the pending ends: %v", err)
		return
	}
	for _, rec := range records {
		d.startSandboxSeatEndStep(rec.Rig, rec.Name, func() {
			d.sandboxSeatEnder()(rec.Rig, rec.Name, rec.Landing)
		})
	}
}

// stopSandboxSeatEnds refuses new seat ends and waits, bounded, for the ones in
// flight. Shutdown calls it before it stops Dolt: the remaining steps of an end
// read beads through Dolt.
func (d *Daemon) stopSandboxSeatEnds() {
	d.beginSandboxSeatEndStop()
	d.waitForSandboxSeatEnds()
}

// beginSandboxSeatEndStop closes the door on new seat ends. It returns once no
// start can add to the count the wait below joins.
func (d *Daemon) beginSandboxSeatEndStop() {
	d.sandboxSeatEndMu.Lock()
	d.sandboxSeatEndStopped = true
	d.sandboxSeatEndMu.Unlock()
}

// waitForSandboxSeatEnds joins the seat ends in flight, for at most
// sandboxSeatEndJoinBudget. An end that outlives the bound is left to the
// process exit: it has not finished its removal, so the seat keeps whatever it
// had not yet deleted, which is the safe answer, and the log says so.
func (d *Daemon) waitForSandboxSeatEnds() {
	done := make(chan struct{})
	go func() {
		d.sandboxSeatEndCycles.Wait()
		close(done)
	}()
	timer := time.NewTimer(sandboxSeatEndJoinBudget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("sandbox seat end: still running after %s; leaving it to the process exit", sandboxSeatEndJoinBudget)
	}
}

// runSandboxSeatEnd reads the landing record that proves w landed and hands the
// seat to the end step. A record that cannot be read is an unlanded answer: the
// seat is kept, because the record is the only proof its work survived.
//
// The read is safe beside the landing worker's appends: w's record was written
// before the after-landing hook ran, and the file's reader skips a torn line
// rather than failing on it, so a record another landing is writing at this
// moment cannot hide w's (see the torn-line case in the test).
func (d *Daemon) runSandboxSeatEnd(rigName string, landings *land.LandingsFile, w land.Work) {
	name := strings.TrimSpace(w.Worker)
	if name == "" {
		return // a crew landing has no seat
	}
	if landings == nil {
		d.logger.Printf("sandbox seat end %s/%s: kept: no landing record to read", rigName, name)
		return
	}
	_, landed, err := landings.Find(w.BeadID, w.Head)
	if err != nil {
		d.logger.Printf("sandbox seat end %s/%s: kept: cannot read the landing record: %v", rigName, name, err)
		return
	}
	d.sandboxSeatEnder()(rigName, name, sandboxSeatLanding{
		BeadID: w.BeadID, Branch: w.Branch, Submitted: w.Submitted, Landed: landed,
	})
}

// sandboxSeatEnder is the step that ends one sandbox seat: patrolScanHost's
// endFinishedSandboxSeat when the daemon was not given one, over the daemon's
// own agent-bead and gateway seams.
func (d *Daemon) sandboxSeatEnder() func(rig, name string, landing sandboxSeatLanding) bool {
	if d.endSandboxSeat != nil {
		return d.endSandboxSeat
	}
	h := &patrolScanHost{d: d, sandboxExists: d.sandboxExists, openRecoveryBeads: d.openRecoveryBeads}
	return h.endFinishedSandboxSeat
}
