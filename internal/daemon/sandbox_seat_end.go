package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/jev/battery"
	jevrules "github.com/steveyegge/gastown/internal/jev/rules"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// A sandbox seat is single-use. Its work lives in the sandbox: the sling's host
// worktree is a snapshot the seat's commits never reach, and once the sandbox
// is gone and the work has landed the only host reader left is the rules
// battery, which asks about the session by reading its transcript. The step
// below removes the worktree once that last ask has happened, and a tmux seat
// keeps its worktree and is never touched here (gt-sefzt).

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
	// SandboxErrored is whether the sandbox the gateway still has is in a
	// terminal failure phase. It does not keep the seat by itself: once every
	// other fact lets the seat go, that sandbox is deleted before the host
	// directories are removed (gt-jhoi1).
	SandboxErrored bool
	// Gateway is the gateway the town's seat config names for the seat's sandbox:
	// the one an errored sandbox is deleted through when its record is gone.
	Gateway string
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

// sandboxNotGoneReason is the keep reason of a seat whose measured sandbox the
// gateway has not confirmed gone. The landing's own end deletes such a sandbox
// before this reason is asked (gt-0vb2fy); one it leaves is the patrol's.
const sandboxNotGoneReason = "the gateway has not confirmed the sandbox gone"

// sandboxSessionLiveReason is the keep reason of a seat whose live session still
// answers to its name. A set ended marker alone does not prove that session has
// ended, so the sandbox waits for the patrol, which runs the same check
// (gt-0vb2fy).
const sandboxSessionLiveReason = "session still live"

// outboxNotCopiedReason is the keep reason of a seat whose outbox could not be
// copied out of its sandbox: the sandbox holds the only copy of what the seat
// wrote, so the delete waits for a copy that works (gt-c6tofd).
const outboxNotCopiedReason = "its outbox is not copied out of its sandbox, so nothing it wrote is deleted with it"

// outboxNotReadReason is the keep reason of a seat whose copied-out outbox, or
// the bead it is applied to, cannot be read: nothing is applied from a read
// that failed, and the copy waits for one that works (gt-s4gfph).
const outboxNotReadReason = "its outbox copy or its bead cannot be read, so the result it holds is not applied"

// outboxEscalationReason is the keep reason of a seat whose outbox's HELP
// request the store did not file as an escalation bead. That request is what
// the outbox exists for, so the sandbox stays until a later pass files it
// (gt-s4gfph).
const outboxEscalationReason = "its outbox's escalation request is not filed as an escalation bead"

// outboxReadyReason is the keep reason of a seat whose outbox's submission is
// not marked ready to land. The landing worker finds a submission by that block
// and the ready label, so a seat whose write failed stays until a later pass
// makes it rather than ending with the submission dropped (gt-qk3ppr).
const outboxReadyReason = "its outbox's submission is not marked ready to land"

// outboxApplyReason is the keep reason of a seat whose outbox the bead store
// did not write in full. A write the store refused is retried by a later pass,
// so the sandbox stays until the bead carries the seat's result rather than
// ending with the refusal named only in a log (gt-kj60e4).
const outboxApplyReason = "its outbox's result is not written to its bead in full"

// keepReason names why the seat's host directories must stay, or "" when they
// may be removed. Every reason is a fact someone else measured; the first one
// this file can answer decides. A missing fact keeps the seat: the removal
// destroys the only host copy of whatever the seat did, so it waits for an
// answer it can trust (gt-sefzt).
func (e sandboxSeatEnd) keepReason() string {
	switch {
	case !e.SandboxSeat:
		return "not a sandbox seat: a tmux seat keeps its worktree"
	case !e.SandboxGone && !e.SandboxErrored:
		return sandboxNotGoneReason
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
		return retiredRecordKeepReason
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
		// Nothing was measured, so no pass may still offer this seat's end; a
		// record it left goes with the window that could have kept the seat.
		h.expirePendingSeatEnd(rig, name)
		log("kept: cannot read its agent bead: %v", err)
		return false
	}
	if fields == nil {
		log("kept: no agent bead, so this town does not track it as a seat")
		h.clearPendingSeatEnd(rig, name)
		h.clearKeptSeat(rig, name)
		return false
	}
	if _, ok, err := h.openshellSeatFor(fields.Agent); err != nil {
		h.expirePendingSeatEnd(rig, name)
		log("kept: %v", err)
		return false
	} else if !ok {
		// A tmux seat keeps today's behavior, silently. Whatever a later
		// pass recorded about the seat is answered: it is not a sandbox seat.
		h.clearPendingSeatEnd(rig, name)
		h.clearKeptSeat(rig, name)
		return false
	}
	// The end keeps deciding on the agent bead's hook; the shadow line measures
	// it against the seat's intent record, so the two can be compared over the
	// shadow window P1.7 opens before the decision moves onto the record
	// (docs/plans/2026-11-15-agent-bead-retirement-plan.md). Nothing it logs
	// changes what this function returns (gt-xoj72t).
	h.shadowWorkBead(rig, name, strings.TrimSpace(fields.HookBead))
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
		// Everything the seat wrote leaves the sandbox before the sandbox does:
		// its outbox here, and its transcripts in the delete below. Both calls
		// this pass can make to delete a sandbox follow this step, so a copy that
		// fails keeps the seat and leaves the only copy of the seat's words where
		// it is (gt-c6tofd).
		if !end.SandboxGone {
			if why := h.copySeatOutbox(rig, name, end.Gateway); why != "" {
				kept = why
				h.clearPendingSeatEnd(rig, name)
				return nil
			}
		}
		// The outbox is applied as soon as it is on the host, whatever keeps
		// the seat afterwards: its HELP request is the seat's ask for a human
		// at the moment it ended, so it is filed before any delete can make the
		// seat wait a whole patrol tick (gt-s4gfph).
		if why := h.applySeatOutbox(rig, name, landing.BeadID); why != "" {
			kept = why
			h.clearPendingSeatEnd(rig, name)
			return nil
		}
		// A sandbox the gateway still holds is ended here, at the landing, once
		// every other fact releases the seat (gt-0vb2fy). The probe takes that
		// sandbox as gone, so a seat kept for any other reason keeps it for the
		// patrol, as before. The start is read before the held sandbox's record
		// is removed, while the record still holds it (gt-6qc3nw.12).
		probe := end
		probe.SandboxGone = true
		held := !end.SandboxGone && !end.SandboxErrored && probe.keepReason() == ""
		var (
			start    time.Time
			startErr error
		)
		if held {
			start, startErr = h.seatStartedAt(rig, name, landing.Submitted)
			if why := h.endHeldSandboxAtLanding(rig, name); why != "" {
				kept = why
				h.clearPendingSeatEnd(rig, name)
				return nil
			}
			end.SandboxGone = true
		}
		if why := end.keepReason(); why != "" {
			kept = why
			// The record and the decision that wrote it are one fact, so both
			// go under the seat lock the removal below is decided under: a
			// heartbeat pass never reads a record this end has already
			// discarded, nor misses one it has just decided to keep
			// (gt-381oj).
			if why == rulesPendingReason {
				h.recordPendingSeatEnd(rig, name, landing)
			} else {
				h.clearPendingSeatEnd(rig, name)
			}
			return nil
		}
		// The seat's start is read before the errored sandbox below is deleted,
		// since its record is the only place the start is kept (gt-6qc3nw.12).
		// A record the teardown removed earlier leaves its start on the ledger
		// (gt-kovllv). A held sandbox's start was read above, before its record
		// went.
		if !held {
			start, startErr = h.seatStartedAt(rig, name, landing.Submitted)
		}
		// Every guard has let the seat go, so its errored sandbox holds no work
		// the landing does not already have. It is deleted before the host
		// directories, so the seat name is free once they are gone (gt-jhoi1).
		if end.SandboxErrored {
			if why := h.removeErroredSeatSandbox(rig, name, end.Gateway); why != "" {
				kept = why
				h.clearPendingSeatEnd(rig, name)
				return nil
			}
		}
		// The usage record is written before the transcripts' worktree goes. A
		// failure is logged and the removal goes on (gt-6qc3nw.12).
		written, incomplete, usageErr := h.writeSeatUsage(rig, name, landing, start, startErr)
		switch {
		case usageErr != nil && written == "":
			log("its usage record is not written: %v", usageErr)
		case usageErr != nil:
			// The file is there and the row is not: the record still reads, so
			// the line names the path rather than calling the record missing.
			log("wrote its usage record %s for bead %s, but %v", written, landing.BeadID, usageErr)
		case written != "" && incomplete != nil:
			log("wrote its usage record %s for bead %s, but its transcripts were not read in full: %v", written, landing.BeadID, incomplete)
		case written != "":
			log("wrote its usage record %s for bead %s", written, landing.BeadID)
		}
		deleted, err := h.removeSeatDirs(rigPath, rig, name)
		if err != nil {
			// A removal that deleted something and then failed has not kept
			// the seat: saying so would send an operator looking for a seat
			// that is half gone (gt-sefzt). Either way nothing is left to
			// wait for, so the record goes.
			if deleted {
				partial = err.Error()
			} else {
				kept = "removing its host directories: " + err.Error()
			}
			h.clearPendingSeatEnd(rig, name)
			return nil
		}
		removed = true
		h.clearPendingSeatEnd(rig, name)
		return nil
	})
	switch {
	case err != nil:
		// The lock itself failed, so nothing was measured: the seat is neither
		// kept nor removed, and its record goes with the window that could
		// have kept it, so it is not offered for ever (gt-381oj).
		h.expirePendingSeatEnd(rig, name)
		log("kept: %v", err)
		return false
	case partial != "":
		// A seat whose directories were partly removed and then failed to
		// finish has nothing left to wait for, so its kept record goes with it
		// (gt-sefzt).
		h.clearKeptSeat(rig, name)
		log("its host directories were partly removed, then the removal failed: %s (the seat is finished: landed %s)", partial, landing.BeadID)
		return false
	case unmeasured != "":
		// A fact that could not be read is retried by a later pass, but only
		// while the window that would have kept the seat is open (gt-381oj).
		h.expirePendingSeatEnd(rig, name)
		log("kept: %s", unmeasured)
		return false
	case kept == rulesPendingReason:
		// The landing's own run of this step is spent, so the seat was
		// recorded under the lock with its landing facts and is offered again
		// on later passes until the ask has happened or the window has closed
		// (gt-381oj). A kept record from an earlier pass names a reason the
		// seat no longer has, so it goes; the rules keep has its own record.
		h.clearKeptSeat(rig, name)
		log("kept: %s", kept)
		return false
	case kept != "":
		h.recordKeptSeat(rig, name, landing, kept)
		log("kept: %s", kept)
		return false
	case removed:
		h.clearKeptSeat(rig, name)
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
	write := h.writePendingSeatEnd
	if write == nil {
		write = writePendingSandboxSeatEnd
	}
	if err := write(h.town(), rig, name, landing); err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot record the seat for a later rules pass: %v; the seat is kept, and only a later landing on the same polecat offers it a pass again", rig, name, err)
	}
}

// clearPendingSeatEnd forgets a record this end has just decided on.
func (h *patrolScanHost) clearPendingSeatEnd(rig, name string) {
	if err := removePendingSandboxSeatEnd(h.town(), rig, name); err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot remove its pending record: %v", rig, name, err)
	}
}

// recordKeptSeat leaves the record of a keep: the bead, the reason the end gave,
// and the landing facts the end decided with, so the patrol can offer the seat's
// end again once it has deleted the sandbox that kept it (gt-cz5tdz). A keep
// whose reason is unchanged keeps the since its first pass stamped, so the
// record ages from the start of the keep; a changed reason starts a new since.
// A record it cannot read, and a record whose since is zero, are logged and
// dated from now: a keep with no start would never age into an attention item.
// A write that fails is logged, and the keep stands (gt-464nw6).
func (h *patrolScanHost) recordKeptSeat(rig, name string, landing sandboxSeatLanding, reason string) {
	path, err := keptSandboxSeatPath(h.town(), rig, name)
	if err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot record the keep: %v", rig, name, err)
		return
	}
	since := h.d.clk().Now().UTC()
	prev, err := readKeptSandboxSeatRecord(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// The first pass of this keep.
	case err != nil:
		h.d.logger.Printf("sandbox seat end %s/%s: its kept record is unreadable, so the keep is dated from now: %v", rig, name, err)
	case prev.Reason == reason && !prev.Since.IsZero():
		since = prev.Since
	case prev.Reason == reason:
		h.d.logger.Printf("sandbox seat end %s/%s: its kept record carries no since, so the keep is dated from now", rig, name)
	}
	rec := keptSandboxSeatRecord{Bead: landing.BeadID, Reason: reason, Since: since, Landing: &landing}
	if err := writeKeptSandboxSeat(h.town(), rig, name, rec); err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot record the keep: %v; the seat is kept all the same", rig, name, err)
	}
}

// clearKeptSeat forgets the kept record of a seat the end has decided is no
// longer kept. A removal that fails is logged, and the decision stands.
func (h *patrolScanHost) clearKeptSeat(rig, name string) {
	h.d.clearGatewayTally(rig, name)
	if err := removeKeptSandboxSeat(h.town(), rig, name); err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot remove its kept record: %v", rig, name, err)
	}
}

// expirePendingSeatEnd forgets a record once its one-hour window has closed. An
// end that measured nothing keeps the seat, but that answer cannot wait for the
// rules ask: past rulesPendingWindow the ask can no longer be pending, so a
// record left behind could never end the seat and would only be offered again
// on every heartbeat for ever. A record still inside its window stays, so the
// next pass retries an end that could not be measured this time (gt-381oj).
func (h *patrolScanHost) expirePendingSeatEnd(rig, name string) {
	path, err := pendingSandboxSeatEndPath(h.town(), rig, name)
	if err != nil {
		return // a name no path can hold has no record
	}
	rec, ok := readPendingSandboxSeatEnd(path)
	if !ok {
		return // no record, or one this pass cannot read (reported when the store is listed)
	}
	if h.d.clk().Now().Sub(rec.Landing.Submitted) < rulesPendingWindow {
		return // the ask can still happen: the next heartbeat offers the seat again
	}
	h.clearPendingSeatEnd(rig, name)
}

// measureSandboxSeatEnd reads the facts the seat's end decides on. It runs
// under the seat's lock, so each fact is the one a sling that holds the lock
// would see. A fact that cannot be read comes back as the reason the seat is
// kept, and nothing is decided from a partial read. A gateway that does not
// answer also leaves the seat's kept record with the landing facts, so the patrol
// retries the gateway on each tick and can finish the seat's end (gt-7h38pd).
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
			h.recordGatewayNoAnswer(rig, name, landing, false)
			return sandboxSeatEnd{}, fmt.Sprintf("cannot ask gateway %q whether the sandbox is gone: %v", seat.gateway, err)
		}
		end.SandboxGone = !exists
		end.Gateway = seat.gateway
		if exists {
			failed, err := h.sandboxFailed(seat.gateway, h.sessionName(rig, name))
			if err != nil {
				h.recordGatewayNoAnswer(rig, name, landing, false)
				return sandboxSeatEnd{}, fmt.Sprintf("cannot ask gateway %q for the sandbox's phase: %v", seat.gateway, err)
			}
			end.SandboxErrored = failed
		}
		// Both questions were answered, so the gateway is no longer silent.
		h.d.clearGatewayTally(rig, name)
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
// A cursor this town cannot read is not pending either, and it says so on
// every measurement, with a line naming the seat and the error, so an
// unreadable cursor never looks like a clean one. Releasing the seat on that
// answer is deliberate and the bead asks for it: only a fact this step measured
// keeps a seat, and the seat that is released here loses its transcript for the
// rules battery. A cursor not written yet is not pending and says nothing: the
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

// copySeatOutbox copies the seat's outbox files out of its sandbox to the host,
// before either delete step of this end can remove that sandbox. It returns the
// reason the seat is kept, or "" when the outbox is on the host, the seat wrote
// none, or its sandbox is already gone.
//
// A record that names the sandbox is copied from as it stands; a seat with no
// record is copied from the gateway the end measured its sandbox at, the record
// the errored-sandbox delete builds when none names it. A record that cannot be
// read keeps the seat: the delete steps that follow read the record again, and
// a read that succeeds then would delete a sandbox whose outbox this step never
// copied (gt-p0nzjw). A copy that fails keeps the seat, naming the copy failure
// (gt-c6tofd).
func (h *patrolScanHost) copySeatOutbox(rig, name, gateway string) string {
	sessionName := h.sessionName(rig, name)
	rec, recorded, err := session.ReadSandboxRecord(h.town(), sessionName)
	if err != nil {
		return "its sandbox record cannot be read, so its outbox is not copied out: " + err.Error()
	}
	if !recorded {
		rec = session.SandboxRecord{Session: sessionName, Rig: rig, Polecat: name, Gateway: gateway}
	}
	return h.copySeatOutboxFor(rec)
}

// copySeatOutboxFor copies the outbox of the sandbox rec names, from the record
// as it stands, and returns the reason the seat is kept when the copy fails.
func (h *patrolScanHost) copySeatOutboxFor(rec session.SandboxRecord) string {
	copyOut := h.sandboxOutboxCopy
	if copyOut == nil {
		copyOut = session.CopySandboxSeatOutbox
	}
	if err := copyOut(h.town(), rec); err != nil {
		return outboxNotCopiedReason + ": " + err.Error()
	}
	return ""
}

// The two files a seat's outbox holds, under the copy directory
// session.CopySandboxSeatOutbox writes them into. notes.md is outbox.NotesFile,
// the name the seat writes it under and the name a file-mode `gt bead note`
// appends to.
const (
	seatOutboxResultFile = "result.json"
	seatOutboxNotesFile  = outbox.NotesFile
)

// seatEscalationSource is the source an outbox's escalation is filed under.
const seatEscalationSource = "sandbox-seat-end"

// appliedSeatOutbox records what a pass applied from a seat's outbox, so a seat
// end that runs again for the same seat writes nothing twice: the notes, the
// ticks and the findings a pass wrote are not written again, and neither is an
// escalation a pass filed. Ready says the ready step is settled: the block was
// written, origin refused it, or the outbox submitted nothing (gt-e1sd0v). A
// record of another bead, or of other bytes, is another run of the same polecat
// name and is applied (gt-s4gfph, gt-qk3ppr).
type appliedSeatOutbox struct {
	Bead      string `json:"bead"`
	Digest    string `json:"digest"`
	Escalated bool   `json:"escalated"`
	Ready     bool   `json:"ready"`
	// Pending names the writes the store refused, so a later pass makes those
	// alone rather than repeating the notes and findings an earlier pass wrote.
	// A record without it predates the field: only the two writes above are
	// pending then (gt-kj60e4).
	Pending []string `json:"pending,omitempty"`
}

// appliedSeatOutboxDir is <town>/.runtime/sandbox-seat-outbox.
func appliedSeatOutboxDir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "sandbox-seat-outbox")
}

// appliedSeatOutboxPath names the record of one seat, refusing a rig or polecat
// name a path cannot hold.
func appliedSeatOutboxPath(townRoot, rig, name string) (string, error) {
	if err := polecat.CheckSandboxSeatNames(rig, name); err != nil {
		return "", err
	}
	return filepath.Join(appliedSeatOutboxDir(townRoot), rig, name+".json"), nil
}

// readAppliedSeatOutbox returns the seat's record, false when it has none, and
// an error for a record that exists and cannot be read.
func readAppliedSeatOutbox(townRoot, rig, name string) (appliedSeatOutbox, bool, error) {
	path, err := appliedSeatOutboxPath(townRoot, rig, name)
	if err != nil {
		return appliedSeatOutbox{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return appliedSeatOutbox{}, false, nil
	}
	if err != nil {
		return appliedSeatOutbox{}, false, err
	}
	var rec appliedSeatOutbox
	if err := json.Unmarshal(data, &rec); err != nil {
		return appliedSeatOutbox{}, false, fmt.Errorf("decoding %s: %w", path, err)
	}
	return rec, true, nil
}

// seatOutboxDigest is the identity of one outbox's bytes: two passes of one
// seat's end read the same bytes, and a run that wrote the same two files again
// has nothing new to report.
func seatOutboxDigest(result, notes []byte) string {
	sum := sha256.New()
	sum.Write(result)
	sum.Write([]byte{0}) // notes.md may begin with the bytes result.json ends with
	sum.Write(notes)
	return hex.EncodeToString(sum.Sum(nil))
}

// applySeatOutbox applies the outbox a seat wrote, copied out of its sandbox to
// the host, through the town's bead store. It returns the reason the seat is
// kept, or "" when the outbox is applied, the seat wrote none, or the validator
// refused it.
//
// An outbox the validator refuses is logged and not applied, and the seat is not
// kept for it: the sandbox holds the same bytes, so a later pass would refuse
// them the same way, and the copy the end read stays on the host for an
// operator.
//
// The writes happen once per outbox. A pass records which ones it made and
// which the store refused, so a seat the patrol offers again makes only the
// ones still pending and does not repeat the notes and findings the earlier
// pass wrote. A failed escalation, a failed ready write, or any other write the
// store refused keeps the seat: the seat's result is on the host copy, while a
// request for help nobody files, a submission nothing marks, and a note nothing
// writes are gone with the sandbox (gt-s4gfph, gt-qk3ppr, gt-kj60e4).
func (h *patrolScanHost) applySeatOutbox(rig, name, beadID string) string {
	dir := session.SandboxOutboxCopyDir(h.town(), rig, name)
	result, err := os.ReadFile(filepath.Join(dir, seatOutboxResultFile))
	if errors.Is(err, os.ErrNotExist) {
		// A seat that wrote no result.json reports nothing, and one that wrote
		// notes without ending is reported here rather than dropped in silence:
		// the notes are the seat's words, and they stay in the copy (gt-s4gfph).
		if _, notesErr := os.Stat(filepath.Join(dir, seatOutboxNotesFile)); notesErr == nil {
			h.d.logger.Printf("sandbox seat end %s/%s: its outbox holds %s but no %s, so nothing is applied; the copy stays at %s", rig, name, seatOutboxNotesFile, seatOutboxResultFile, dir)
		}
		return ""
	}
	if err != nil {
		return outboxNotReadReason + ": " + err.Error()
	}
	notes, err := os.ReadFile(filepath.Join(dir, seatOutboxNotesFile))
	if errors.Is(err, os.ErrNotExist) {
		notes = nil // notes.md is optional: a seat that wrote none wrote no notes
	} else if err != nil {
		return outboxNotReadReason + ": " + err.Error()
	}
	decoded, err := outbox.Validate(result, notes, beadID)
	if err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: its outbox is not applied: %v; the copy it was read from stays at %s", rig, name, err, dir)
		return ""
	}
	digest := seatOutboxDigest(result, notes)
	seat := h.seat(rig, name)
	applied, haveApplied, appliedErr := readAppliedSeatOutbox(h.town(), rig, name)
	if appliedErr != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: its applied record cannot be read, so its outbox is applied again: %v", rig, name, appliedErr)
	}
	if appliedErr == nil && haveApplied && applied.Bead == beadID && applied.Digest == digest {
		// An earlier pass applied this outbox. Its record names which writes are
		// still pending; the others are not repeated (gt-s4gfph, gt-qk3ppr).
		rec := appliedSeatOutbox{
			Bead:      beadID,
			Digest:    digest,
			Escalated: applied.Escalated || decoded.Escalation == nil,
			Ready:     applied.Ready || decoded.Submission == nil,
			Pending:   applied.Pending,
		}
		if rec.Escalated && rec.Ready && len(rec.Pending) == 0 {
			return "" // this outbox was applied whole on an earlier pass
		}
		return h.finishSeatOutbox(rig, name, beadID, seat, decoded, rec)
	}
	bd := h.recoveryBeads()
	issue, err := bd.Show(beadID)
	if err != nil {
		return outboxNotReadReason + ": cannot read its bead: " + err.Error()
	}
	store := &seatOutboxStore{bd: bd, escalate: h.fileSeatEscalation}
	applyErr := outbox.Apply(store, beadID, outbox.Seat{Actor: seat.Address(), Worker: seat.Name}, beads.AcceptanceText(issue), decoded, h.submissionOrigin(rig, decoded))
	if applyErr != nil {
		// Never swallowed: the copy on the host is where an operator finds what
		// the bead did not get. Every write the store refused keeps the seat, and
		// the record below names each one a later pass makes again.
		h.d.logger.Printf("sandbox seat end %s/%s: its outbox is not applied in full: %v; the copy it was read from stays at %s", rig, name, applyErr, dir)
	}
	// A refusal never reaches the store: origin's final answer settles the ready
	// step without a block, while a failed read keeps the seat (gt-e1sd0v).
	readyErr := store.readyErr
	var readErr *outbox.OriginReadError
	if readyErr == nil && errors.As(applyErr, &readErr) {
		readyErr = readErr
	}
	h.recordAppliedSeatOutbox(rig, name, appliedSeatOutbox{
		Bead:      beadID,
		Digest:    digest,
		Escalated: store.escalationErr == nil,
		Ready:     readyErr == nil,
		Pending:   store.pending,
	})
	if store.escalationErr != nil {
		return outboxEscalationReason + ": " + store.escalationErr.Error()
	}
	if readyErr != nil {
		return outboxReadyReason + ": " + readyErr.Error()
	}
	// A failure that left no write pending is the outbox's own values being
	// refused, not a write the store lost: a later pass would refuse them the
	// same way, so the seat is not kept for it, as for the validator's refusal.
	if len(store.pending) > 0 {
		return outboxApplyReason + ": " + applyErr.Error()
	}
	return ""
}

// finishSeatOutbox makes the writes an earlier pass recorded as unwritten: the
// outbox's escalation request, the READY TO LAND block of its submission, and
// the writes its Pending names. rec is recorded again with the ones this pass
// made. It returns the keep reason of a write that failed again, or "" when each
// pending write is written. The notes, ticks, findings and status the earlier
// pass wrote are not written again: the retry applies the outbox through a store
// that skips every write outside Pending (gt-s4gfph, gt-kj60e4).
//
// The ready retry reads origin again: origin's refusal settles the step and is
// never asked twice, while a failed read or write keeps the seat (gt-e1sd0v).
func (h *patrolScanHost) finishSeatOutbox(rig, name, beadID string, seat supervisor.Seat, decoded outbox.Result, rec appliedSeatOutbox) string {
	if !rec.Escalated {
		if err := h.fileSeatEscalation(beadID, seat.Address(), decoded.Escalation.Text); err != nil {
			return outboxEscalationReason + ": " + err.Error()
		}
		rec.Escalated = true
	}
	if !rec.Ready {
		store := &seatOutboxStore{bd: h.recoveryBeads(), escalate: h.fileSeatEscalation}
		who := outbox.Seat{Actor: seat.Address(), Worker: seat.Name}
		err := outbox.MarkReady(store, beadID, who, decoded, h.submissionOrigin(rig, decoded))
		var refusal *outbox.ReadyRefusal
		switch {
		case errors.As(err, &refusal):
			// Origin's answer is final: the block is never written, and the
			// record settles the step rather than asking again (gt-e1sd0v).
			h.d.logger.Printf("sandbox seat end %s/%s: its submission gets no %s block: %v", rig, name, land.ReadyNoteMarker, err)
		case err != nil:
			// The record keeps the ready write pending, so the next pass
			// retries it; the escalation this pass filed stays recorded.
			h.recordAppliedSeatOutbox(rig, name, rec)
			return outboxReadyReason + ": " + err.Error()
		}
		rec.Ready = true
	}
	if len(rec.Pending) > 0 {
		bd := h.recoveryBeads()
		issue, err := bd.Show(beadID)
		if err != nil {
			return outboxNotReadReason + ": cannot read its bead: " + err.Error()
		}
		store := &seatOutboxStore{bd: bd, escalate: h.fileSeatEscalation, only: seatOutboxWriteSet(rec.Pending)}
		who := outbox.Seat{Actor: seat.Address(), Worker: seat.Name}
		rest := decoded
		rest.Submission = nil // the ready write is made above or settled, never pending
		if err := outbox.Apply(store, beadID, who, beads.AcceptanceText(issue), rest, outbox.OriginTip{}); err != nil {
			// The record keeps the writes this pass could not make pending, so
			// the next pass makes those alone; those it made stay recorded.
			rec.Pending = store.pending
			h.recordAppliedSeatOutbox(rig, name, rec)
			return outboxApplyReason + ": " + err.Error()
		}
		rec.Pending = nil
	}
	h.recordAppliedSeatOutbox(rig, name, rec)
	return ""
}

// submissionOrigin is origin's answer for the branch a seat's outbox submitted,
// read from the rig's shared bare repository: the one repository of the rig no
// polecat writes, and a submission Apply refuses from its own fields never
// reaches it. A branch origin names with no commit is an answer, not a failed
// read, so it is refused like an absent branch (gt-e1sd0v).
func (h *patrolScanHost) submissionOrigin(rig string, r outbox.Result) outbox.OriginTip {
	if r.Submission == nil || outbox.SubmissionProblem(r.Submission, r.Status) != nil {
		return outbox.OriginTip{}
	}
	g := h.d.gitAt(constants.RigBareRepoPath(filepath.Join(h.town(), rig)))
	sha, held, err := originBranchRead(g, r.Submission.Branch)
	switch {
	case errors.Is(err, errOriginNoCommit):
		return outbox.OriginTip{Held: true}
	case err != nil:
		return outbox.OriginTip{Err: err}
	}
	return outbox.OriginTip{Head: sha, Held: held}
}

// seatOutboxWriteSet is the lookup a retry store skips every write outside of.
func seatOutboxWriteSet(pending []string) map[string]bool {
	set := make(map[string]bool, len(pending))
	for _, write := range pending {
		set[write] = true
	}
	return set
}

// recordAppliedSeatOutbox records what a pass applied. A write that fails is
// logged: the next pass applies the outbox again, which can repeat a note or a
// finding, and that is better than a record saying it was applied (gt-s4gfph).
func (h *patrolScanHost) recordAppliedSeatOutbox(rig, name string, rec appliedSeatOutbox) {
	path, err := appliedSeatOutboxPath(h.town(), rig, name)
	if err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot name its applied record: %v; a later pass applies its outbox again", rig, name, err)
		return
	}
	if err := atomicfile.EnsureDirAndWriteJSON(path, rec); err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: cannot record its applied outbox: %v; a later pass applies it again", rig, name, err)
	}
}

// seatOutboxStore is outbox.Store over the town's bead store: the writes a
// copied-out outbox asks for, and the seat's HELP request filed as an escalation
// bead. escalationErr is the escalation write's own failure and readyErr the
// ready write's; Apply joins them with the others, and each one alone keeps the
// seat, so a write the bead did not get is made again (gt-s4gfph, gt-qk3ppr).
// pending names the other writes the store refused, for the applied record that
// keeps the seat for them (gt-kj60e4).
type seatOutboxStore struct {
	bd            beads.Client
	escalate      func(beadID, actor, text string) error
	escalationErr error
	readyErr      error
	// only names the writes a retry makes. When it is set, every write outside
	// it is skipped, so the retry does not repeat what an earlier pass wrote.
	only map[string]bool
	// pending names the writes that returned an error, in the order Apply made
	// them.
	pending []string
	// findings counts AddComment calls, so a refused finding is named by the
	// index Apply read it at.
	findings int
}

// The writes an outbox application makes, as the applied record names the ones
// a retry still has to make (gt-kj60e4).
const (
	seatOutboxTitleWrite      = "title"
	seatOutboxNotesWrite      = "notes"
	seatOutboxAcceptanceWrite = "acceptance"
	seatOutboxStatusWrite     = "status"
	// seatOutboxFindingWrite is suffixed with the finding's index in the
	// result, since each finding is a write of its own.
	seatOutboxFindingWrite = "finding"
)

// write makes one write of the outbox, skips it when a retry names others, and
// records a failure so a later pass makes just that one again.
func (s *seatOutboxStore) write(kind string, fn func() error) error {
	if s.only != nil && !s.only[kind] {
		return nil
	}
	if err := fn(); err != nil {
		s.pending = append(s.pending, kind)
		return err
	}
	return nil
}

// SetTitle replaces the bead's title with the one the seat ended with.
func (s *seatOutboxStore) SetTitle(beadID, title string) error {
	return s.write(seatOutboxTitleWrite, func() error {
		return s.bd.Update(beadID, beads.UpdateOptions{Title: &title})
	})
}

// AppendNotes appends the seat's notes to its bead.
func (s *seatOutboxStore) AppendNotes(beadID, text string) error {
	return s.write(seatOutboxNotesWrite, func() error {
		return s.bd.AppendNotes(beadID, text)
	})
}

// SetAcceptance replaces the bead's acceptance text, with the boxes the seat
// ticked ticked.
func (s *seatOutboxStore) SetAcceptance(beadID, text string) error {
	return s.write(seatOutboxAcceptanceWrite, func() error {
		return s.bd.Update(beadID, beads.UpdateOptions{Acceptance: &text})
	})
}

// AddComment adds one of the seat's findings, under the seat's name.
func (s *seatOutboxStore) AddComment(beadID, author, text string) error {
	i := s.findings
	s.findings++
	return s.write(fmt.Sprintf("%s:%d", seatOutboxFindingWrite, i), func() error {
		return s.bd.AddCommentAs(beadID, author, text)
	})
}

// SetStatus sets the bead's status, the seat's deferred result.
func (s *seatOutboxStore) SetStatus(beadID, status string) error {
	return s.write(seatOutboxStatusWrite, func() error {
		return s.bd.Update(beadID, beads.UpdateOptions{Status: &status})
	})
}

// Escalate files the seat's HELP request as an escalation bead, remembering a
// failure so the caller can keep the seat for it. A retry store makes only the
// writes its record left pending, so the escalation it skips is finishSeatOutbox's
// own write (gt-s4gfph).
func (s *seatOutboxStore) Escalate(beadID, actor, text string) error {
	if s.only != nil {
		return nil
	}
	err := s.escalate(beadID, actor, text)
	if err != nil {
		s.escalationErr = err
	}
	return err
}

// MarkReadyToLand appends the seat's READY TO LAND block and adds the label the
// landing worker picks the bead by in one update, as gt done marks its own
// submission: the block and the label land together or not at all. The rework
// and overseer-review labels are removed with it, because a new head must face
// om again (gt-g8t3m). A write the store reported is read back for the label,
// since bd reporting success is not proof the write persisted (GH#1945): a lost
// label is a submission nothing picks up, and only a later pass retrying this
// write repairs it.
//
// A retry makes this same write — a pass kept the seat for it, or its applied
// record was lost — so the block is appended only when the notes do not already
// carry this submission's block: a second copy would only grow the bead, and the
// label write is made either way. The failure is remembered so the seat's end
// keeps the seat for it, and the retry knows the write is still pending. A retry
// store that names only the other pending writes skips this one, which
// finishSeatOutbox makes itself (gt-s4gfph).
func (s *seatOutboxStore) MarkReadyToLand(beadID, note string) error {
	if s.only != nil {
		return nil
	}
	err := s.markReadyToLand(beadID, note)
	if err != nil {
		s.readyErr = err
	}
	return err
}

// markReadyToLand is MarkReadyToLand without the failure kept.
func (s *seatOutboxStore) markReadyToLand(beadID, note string) error {
	// The block says what a retry must not write twice, and a note this code
	// cannot read as one cannot be checked: refuse it rather than append it
	// blind.
	want, ok := land.ParseReadyNote(note)
	if !ok {
		return fmt.Errorf("the %s block for %s cannot be read as one", land.ReadyNoteMarker, beadID)
	}
	// The bead is read before the write, not only after it: a store that cannot
	// read it refuses here rather than appending a block it cannot check.
	issue, err := s.bd.Show(beadID)
	if err != nil {
		return fmt.Errorf("reading %s before the ready write: %w", beadID, err)
	}
	opts := beads.UpdateOptions{
		AddLabels:    []string{land.LabelReadyToLand},
		RemoveLabels: []string{land.LabelRework, land.LabelOverseerReviewed},
	}
	if !readyBlockWritten(issue.Notes, want) {
		opts.AppendNotes = note
	}
	if err := s.bd.Update(beadID, opts); err != nil {
		return fmt.Errorf("adding %s: %w", land.LabelReadyToLand, err)
	}
	issue, err = s.bd.Show(beadID)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", beadID, err)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		return fmt.Errorf("%s does not carry %s after the write", beadID, land.LabelReadyToLand)
	}
	return nil
}

// readyBlockWritten reports whether the notes' last READY TO LAND block is
// this submission's: the branch, head, target and worker match, so a block a
// retrying pass rebuilds from the same outbox matches the one an earlier pass
// wrote whatever its submitted stamp. The stamp is not part of a submission's
// identity; the head is the commit that lands.
func readyBlockWritten(notes string, want land.Work) bool {
	have, ok := land.ParseReadyNote(notes)
	return ok && have.Branch == want.Branch && have.Head == want.Head &&
		have.Target == want.Target && have.Worker == want.Worker
}

// fileSeatEscalation files one outbox escalation request as an escalation bead
// raised as the seat, about the bead the seat was given. The seat's text is the
// bead's reason; its first line is the title, which is one line, and the
// severity is the one the seat's `gt escalate` wrote into the text (a request
// that names none the config accepts is medium, gt escalate's default).
//
// One open escalation stands for one request about one bead, which its
// fingerprint keys: a raise that finds the same request already open writes
// nothing, so a pass whose applied record was lost, or the patrol's own outbox
// poll, adds no second bead for it (gt-vwry). A later, different request about
// the same bead is a second ask and files its own bead (gt-kj60e4).
func (h *patrolScanHost) fileSeatEscalation(beadID, actor, text string) error {
	bd := h.recoveryBeads()
	title := seatEscalationHeadline(text)
	fingerprint := notify.FingerprintLabel(seatEscalationKey(beadID, text))
	open, err := beads.ListEscalationsByFingerprint(bd, fingerprint)
	if err != nil {
		return fmt.Errorf("looking for the bead's open escalation: %w", err)
	}
	if len(open) > 0 {
		return nil
	}
	_, err = beads.CreateEscalationBead(bd, title, &beads.EscalationFields{
		Severity:    seatEscalationSeverity(text),
		Reason:      text,
		Source:      seatEscalationSource,
		EscalatedBy: actor,
		EscalatedAt: h.d.clk().Now().UTC().Format(time.RFC3339),
		RelatedBead: beadID,
		Fingerprint: fingerprint,
	})
	if err != nil {
		return fmt.Errorf("filing the escalation: %w", err)
	}
	return nil
}

// seatEscalationKey is the alert key one outbox escalation request is filed
// under, whoever files it: the bead it is about and the request's own text. A
// key of the bead alone would take a second, different request for a repeat of
// the first and drop it (gt-kj60e4). The NUL separator keeps the two apart: no
// bead id holds one, so two requests can never spell the same key.
func seatEscalationKey(beadID, text string) string {
	return seatEscalationSource + ":" + beadID + "\x00" + text
}

// seatEscalationHeadline is a request's first line: an escalation bead's title
// is one line, and the whole request travels as its reason.
func seatEscalationHeadline(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = text[:i]
	}
	return text
}

// seatEscalationSeverity reads the severity a seat's `gt escalate` wrote into
// its request text ("Severity: <level>", escalate_impl.go's
// escalationRequestText): the last line that names a level the config accepts.
// The seat's file mode validates the severity before writing it, so the default
// answers a request an earlier writer shaped without one.
func seatEscalationSeverity(text string) string {
	severity := config.SeverityMedium
	for _, line := range strings.Split(text, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "Severity: ")
		if !ok {
			continue
		}
		value = strings.ToLower(strings.TrimSpace(value))
		if config.IsValidSeverity(value) {
			severity = value
		}
	}
	return severity
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

// sandboxFailed reports whether the gateway's sandbox called name is in a
// terminal failure phase. A gateway that cannot answer is an error, never a
// phase.
func (h *patrolScanHost) sandboxFailed(gateway, name string) (bool, error) {
	status := h.sandboxStatus
	if status == nil {
		status = session.GatewaySandboxPhase
	}
	st, err := status(gateway, name)
	if err != nil {
		return false, err
	}
	return session.SandboxPhaseFailed(st.Phase), nil
}

// removeErroredSeatSandbox deletes the errored sandbox of a seat that may go,
// with its record, so the seat name is free. It returns the reason the seat is
// kept, or "" when that sandbox and its record were removed. The gateway is
// asked again through session.CleanupErroredSandboxWithRecord, so a sandbox that
// stopped failing in between keeps the seat for a later pass (gt-jhoi1).
//
// A seat with no record has its sandbox at the gateway the town's seat config
// names, so the delete is keyed by that gateway and the seat's own name rather
// than keeping the seat forever (gt-jhoi1).
func (h *patrolScanHost) removeErroredSeatSandbox(rig, name, gateway string) string {
	status := h.sandboxStatus
	if status == nil {
		status = session.GatewaySandboxPhase
	}
	deleter := h.sandboxDeleter
	if deleter == nil {
		deleter = session.SandboxDeleter
	}
	sessionName := h.sessionName(rig, name)
	rec, recorded, err := session.ReadSandboxRecord(h.town(), sessionName)
	if err != nil {
		return "its errored sandbox is not removed: " + err.Error()
	}
	if !recorded {
		rec = session.SandboxRecord{Session: sessionName, Rig: rig, Polecat: name, Gateway: gateway}
	}
	removed, err := session.CleanupErroredSandboxWithRecord(status, deleter, h.town(), sessionName, rec)
	if err != nil {
		return "its errored sandbox is not removed: " + err.Error()
	}
	if !removed {
		return "its sandbox is no longer in a failure phase"
	}
	return ""
}

// endHeldSandboxAtLanding ends the sandbox a landed seat's gateway still holds,
// once every other fact has released the seat (gt-0vb2fy). It is the patrol's
// own path, endHeldSandbox: the sandbox answers whether its ended marker is set,
// and when it is the record deleter copies the transcripts and deletes the
// sandbox. The record is then dropped, as the patrol drops it.
//
// It returns the reason the seat is kept, or "" when the sandbox and its record
// are gone. It asks the same live-session probe the patrol asks before either
// deletes (sandboxSeatSessionLive), and keeps the sandbox, with the probe's
// reason, when a session still answers to the seat's name. A probe that errors
// keeps it too: the landing has no proof the session is not live, so it fails
// closed. A record that cannot be read or is absent, and every refusal
// endHeldSandbox makes, keep the seat with that refusal's own reason, so the
// patrol deletes the sandbox as it does today.
func (h *patrolScanHost) endHeldSandboxAtLanding(rig, name string) string {
	sessionName := h.sessionName(rig, name)
	live, err := h.sandboxSeatSessionLive(sessionName)
	if err != nil {
		return "cannot ask tmux whether its session is live, so the sandbox is left for the patrol: " + err.Error()
	}
	if live {
		return sandboxSessionLiveReason
	}
	rec, ok, err := session.ReadSandboxRecord(h.town(), sessionName)
	if err != nil {
		return "its sandbox record cannot be read, so the sandbox is left for the patrol: " + err.Error()
	}
	if !ok {
		return "its sandbox record is absent, so no deleter names its sandbox"
	}
	if why := h.endHeldSandbox("sandbox seat end", sessionName, rec); why != "" {
		return why
	}
	if err := h.removeRecord(sessionName); err != nil {
		return "its sandbox is deleted but its record could not be removed: " + err.Error()
	}
	h.d.logger.Printf("sandbox seat end %s/%s: deleted its held sandbox at the landing, not at the next patrol tick", rig, name)
	return ""
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
// The step is named by the seat when the Worker is a polecat's own name on a
// polecat branch, and by the bead when it is not: a takeover lands a crew
// branch, whose Worker is the crew actor, so the bead names the seats the step
// ends (gt-m1qf31). The subject is the whole phrase after "sandbox seat end ",
// so a bead id is never printed where a seat name belongs.
//
// The stop check and the count are one step under sandboxSeatEndMu, the lock
// stopSandboxSeatEnds takes before it waits: an end added after that wait
// started would be one the shutdown never joins, and the two would race.
func (d *Daemon) startSandboxSeatEnd(rigName string, landings *land.LandingsFile, w land.Work) {
	name := strings.TrimSpace(w.Worker)
	if name != "" && polecat.CheckSandboxSeatNames(rigName, name) == nil && !strings.HasPrefix(w.Branch, authorkind.CrewPrefix) {
		d.startSandboxSeatEndStep(rigName+"/"+name, func() {
			d.runSandboxSeatEnd(rigName, landings, w)
		})
		return
	}
	if strings.TrimSpace(w.BeadID) == "" {
		return // a landing with no bead names no seat to resolve
	}
	d.startSandboxSeatEndStep("bead "+w.BeadID, func() {
		d.runSandboxSeatEnd(rigName, landings, w)
	})
}

// startSandboxSeatEndStep runs one seat end off the caller's goroutine, with
// the shutdown accounting every seat end shares: a daemon that is already
// stopping starts none, and stopSandboxSeatEnds joins the ones in flight.
// subject names the end: "<rig>/<seat>" for a seat, "bead <id>" for a landing
// whose seats are read from its bead.
func (d *Daemon) startSandboxSeatEndStep(subject string, run func()) {
	d.sandboxSeatEndMu.Lock()
	if d.sandboxSeatEndStopped || d.ctxOrBackground().Err() != nil {
		d.sandboxSeatEndMu.Unlock()
		d.logger.Printf("sandbox seat end %s: kept: the daemon is stopping", subject)
		return
	}
	d.sandboxSeatEndCycles.Add(1)
	d.sandboxSeatEndMu.Unlock()
	go func() {
		defer d.sandboxSeatEndCycles.Done()
		defer func() {
			if r := recover(); r != nil {
				d.logger.Printf("landing_worker: %s: sandbox seat end panicked: %v", subject, r)
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
		d.startSandboxSeatEndStep(rec.Rig+"/"+rec.Name, func() {
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
// A Worker that is a polecat's own name on its own branch ends that seat. A
// crew branch, or a Worker that is no seat name (a takeover's crew actor, or
// none), reaches no seat as a polecat: the bead names the seats instead (see
// endSandboxSeatsOfBead).
//
// The read is safe beside the landing worker's appends: w's record was written
// before the after-landing hook ran, and the file's reader skips a torn line
// rather than failing on it, so a record another landing is writing at this
// moment cannot hide w's (see the torn-line case in the test).
func (d *Daemon) runSandboxSeatEnd(rigName string, landings *land.LandingsFile, w land.Work) {
	name := strings.TrimSpace(w.Worker)
	if name == "" || polecat.CheckSandboxSeatNames(rigName, name) != nil || strings.HasPrefix(w.Branch, authorkind.CrewPrefix) {
		d.endSandboxSeatsOfBead(rigName, landings, w)
		return
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

// endSandboxSeatsOfBead offers the end to each polecat seat the landed bead was
// built on, with the landing's own facts, so every guard in keepReason still
// decides. A seat the bead names no seat for is left alone; that is one log
// line and no end. The bead is named as "bead <id>" in every line, never in
// the seat-name slot of "sandbox seat end <rig>/<name>" (gt-m1qf31).
func (d *Daemon) endSandboxSeatsOfBead(rigName string, landings *land.LandingsFile, w land.Work) {
	if strings.TrimSpace(w.BeadID) == "" {
		d.logger.Printf("sandbox seat end: kept: the landing names no bead, so no polecat seat is resolved")
		return
	}
	seats, err := landingSeats(d.rigWorkBeads(rigName), rigName, w.BeadID)
	if err != nil {
		d.logger.Printf("sandbox seat end bead %s: kept: %v", w.BeadID, err)
		return
	}
	if landings == nil {
		d.logger.Printf("sandbox seat end bead %s: kept: no landing record to read", w.BeadID)
		return
	}
	_, landed, err := landings.Find(w.BeadID, w.Head)
	if err != nil {
		d.logger.Printf("sandbox seat end bead %s: kept: cannot read the landing record: %v", w.BeadID, err)
		return
	}
	for name, branch := range seats {
		if branch == "" {
			// Only the assignee names this seat: its own branch is the one its
			// agent bead records, and only while that bead still hooks this
			// bead. A seat re-slung to other work is not this bead's seat.
			resolved, reason, err := d.assigneeSeatBranch(rigName, name, w.BeadID)
			switch {
			case err != nil:
				d.logger.Printf("sandbox seat end bead %s: seat %s not offered: cannot read its agent bead: %v", w.BeadID, name, err)
				continue
			case reason != "":
				d.logger.Printf("sandbox seat end bead %s: seat %s not offered: %s", w.BeadID, name, reason)
				continue
			}
			branch = resolved
		}
		d.sandboxSeatEnder()(rigName, name, sandboxSeatLanding{
			BeadID: w.BeadID, Branch: branch, Submitted: w.Submitted, Landed: landed,
		})
	}
}

// assigneeSeatBranch resolves the branch of a seat only the bead's assignee
// names: the one its agent bead records. The seat is this bead's seat while
// that agent bead still hooks beadID, or, after the gt done that cleared the
// hook, while the bead is its last source issue: the seat's own finish is a
// fact the end settles, not the offer (gt-m1qf31, gt-xb20n9). A reason, not an
// error, says why a readable agent bead does not resolve a branch; an
// unreadable one is returned as an error so its caller says which read failed.
func (d *Daemon) assigneeSeatBranch(rig, name, beadID string) (branch, reason string, err error) {
	fields, err := d.sandboxSeatEndHost().seatAgentFields(rig, name)
	if err != nil {
		return "", "", fmt.Errorf("reading the agent bead: %w", err)
	}
	if fields == nil {
		return "", "it has no agent bead", nil
	}
	if reason = seatEndsBead(fields, beadID); reason != "" {
		return "", reason, nil
	}
	if branch = strings.TrimSpace(fields.Branch); branch == "" {
		return "", "its agent bead records no branch", nil
	}
	return branch, "", nil
}

// seatEndsBead is why an assignee-only seat's agent bead does not name beadID
// as its own work, or "" when it does. A set hook must be beadID: a seat a
// sling gave other work is not this bead's seat. An empty hook is the state gt
// done leaves, and it names the bead while the seat's last source issue is
// beadID. A seat whose agent state never reached "done" is offered too: a held
// sandbox is deleted only once its ended marker is set, which
// endHeldSandboxAtLanding asks, and a sandbox the gateway no longer has leaves
// no seat to keep, so the offer makes no gateway call (gt-xb20n9).
func seatEndsBead(fields *beads.AgentFields, beadID string) string {
	hook := strings.TrimSpace(fields.HookBead)
	switch {
	case hook == beadID:
		return ""
	case hook != "":
		return fmt.Sprintf("its agent bead hooks %q", hook)
	}
	if source := strings.TrimSpace(fields.LastSourceIssue); source != beadID {
		return fmt.Sprintf("its agent bead's last source issue is %q, not the landed bead", source)
	}
	return ""
}

// landingSeats maps each polecat seat a landed bead was built on to the branch
// its own READY TO LAND block named, and to "" for a seat only the assignee
// names: the caller resolves that one from its agent bead. Every block is read
// (an earlier attempt of a rework is another seat), and the assignee counts
// when it is <rig>/polecats/<name>. A name that is not one path segment is
// skipped, and a bead left with no seat is an error: the caller ends nothing
// rather than guess (gt-m1qf31).
func landingSeats(reader beadReader, rig, beadID string) (map[string]string, error) {
	if reader == nil {
		return nil, errors.New("no bead reader is wired to resolve its seats")
	}
	issue, err := reader.Show(beadID)
	if err != nil {
		return nil, fmt.Errorf("reading the bead: %w", err)
	}
	if issue == nil {
		return nil, fmt.Errorf("bead %s is not found", beadID)
	}
	seats := map[string]string{}
	add := func(branch string) {
		rest, ok := strings.CutPrefix(branch, land.PolecatBranchPrefix)
		name, tail, ok2 := strings.Cut(rest, "/")
		if !ok || !ok2 || tail == "" || polecat.CheckSandboxSeatNames(rig, name) != nil {
			return
		}
		if _, dup := seats[name]; !dup {
			seats[name] = branch
		}
	}
	for notes := issue.Notes; ; {
		idx := strings.LastIndex(notes, land.ReadyNoteMarker+"\n")
		if idx < 0 {
			break
		}
		if work, ok := land.ParseReadyNote(notes[idx:]); ok {
			add(work.Branch)
		}
		notes = notes[:idx]
	}
	if parts := strings.Split(issue.Assignee, "/"); len(parts) == 3 && parts[0] == rig && parts[1] == "polecats" {
		if _, dup := seats[parts[2]]; !dup && polecat.CheckSandboxSeatNames(rig, parts[2]) == nil {
			seats[parts[2]] = ""
		}
	}
	if len(seats) == 0 {
		return nil, fmt.Errorf("its assignee %q and its READY TO LAND blocks name no polecat seat", issue.Assignee)
	}
	return seats, nil
}

// sandboxSeatEnder is the step that ends one sandbox seat: patrolScanHost's
// endFinishedSandboxSeat when the daemon was not given one, over the daemon's
// own agent-bead and gateway seams.
func (d *Daemon) sandboxSeatEnder() func(rig, name string, landing sandboxSeatLanding) bool {
	if d.endSandboxSeat != nil {
		return d.endSandboxSeat
	}
	return d.sandboxSeatEndHost().endFinishedSandboxSeat
}

// sandboxSeatEndHost is the host the daemon's own seat end runs on. It is the
// production wiring of the seat's transcript lister (seatUsageHook.list) and of
// the town's usage_records store its row is filed in (seatUsageHook.store),
// neither of which the host defaults.
func (d *Daemon) sandboxSeatEndHost() *patrolScanHost {
	return &patrolScanHost{
		d:                 d,
		sandboxExists:     d.sandboxExists,
		openRecoveryBeads: d.openRecoveryBeads,
		seatUsage:         seatUsageHook{list: agentlog.ProjectTranscripts, store: d.usageRecords()},
	}
}
