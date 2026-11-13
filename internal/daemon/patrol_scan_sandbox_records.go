package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

const (
	// sandboxRecordGrace is how old a registry record must be before the reap
	// may judge its sandbox gone. A record is written when a launch's Prepare
	// ends; the grace keeps a sandbox still coming up from being judged.
	sandboxRecordGrace = 5 * time.Minute
	// sandboxRecordsPerTick bounds the records one tick checks.
	sandboxRecordsPerTick = 20
)

// patrolScanSandboxRecords removes the registry record of every sandbox the
// gateway no longer has. A sandbox created with --no-keep deletes itself when
// its main process exits, and its record would otherwise keep the pinned seat
// looking occupied. A record is removed only when all hold, and any one that
// cannot be answered leaves it:
//   - it is at least sandboxRecordGrace old;
//   - no live session answers to its name (the routed tmux client asks the
//     sandbox itself, so a running sandboxed seat is live). A tmux error counts
//     as not live and is logged: it is fail-safe because the gateway's answer is
//     still required next;
//   - the gateway the record names reports no sandbox of that name (an
//     unreachable gateway or any other error is not "gone").
//
// A sandbox the gateway still has is held: its seat ended and its sandbox stays
// up for the hold, with an ended marker in it (gt-yck7hz). Such a record is
// ended here only when no live session answers to its name and the sandbox says
// its marker is set, and then through the record deleter, which copies the
// seat's transcripts before it deletes the sandbox. The delete and the record's
// removal run under the seat lock the landing's own end takes, and the record is
// re-read under it, so the landing and this pass cannot both delete one sandbox
// (gt-0vb2fy). A marker that is absent, an exec that fails, a deleter that
// fails, and a town with no deleter linked each keep the record, so a seat still
// starting is never deleted by this path. A finish whose stop deletes the
// sandbox removes the record too, so by then there is no record for this path to
// find.
//
// A held sandbox the gateway reports Stopped is ended on its phase, not its
// marker, and only when its seat's end is recorded with landing facts that prove
// the work landed. A Stopped sandbox whose work is not proven landed is kept with
// the reason stoppedUnlandedReason (gt-7h38pd). A gateway that does not answer,
// for the existence question or the phase question, is a retry on the next tick:
// a seat whose end is recorded counts the tick in its kept record, whose reason
// names the time of the last try, and nothing else is recorded for it.
//
// At most the budget (sandboxRecordsPerTick) of records is checked per tick,
// live ones included. A checked record costs one tmux probe, and unless a live
// session answers, the existence question to the gateway, then the phase
// question when the sandbox exists: at most two gateway questions. So the walk
// makes at most the budget in tmux probes and twice the budget in gateway
// questions, and records the gateway still has cannot starve the ones behind
// them. A completion (completeKeptSeatEnd) asks the gateway its own two
// questions, and those are exempt from the walk's budget: a completion follows
// a record the pass has just checked, so it takes no other record's place, and
// it cannot wait for a later tick because nothing else offers that kept seat's
// end again. One tick therefore asks at most four times the budget in gateway
// questions and still checks at most the budget in records. The walk over the
// name-sorted records starts after the name the
// last tick ended on (Daemon.sandboxRecordCursor), so consecutive ticks judge
// consecutive names and every record is judged within ceil(n/budget) ticks,
// whatever the tick's clock phase. A failed removal is logged and the pass goes
// on.
//
// A seat the end kept is only released once its sandbox is gone, and a pass
// that removes the record has just made that true; nothing else offers that
// seat's end again, so the pass completes it (gt-cz5tdz). See
// completeKeptSeatEnd.
func (h *patrolScanHost) patrolScanSandboxRecords() {
	names := session.ListSandboxSessions(h.town())
	if len(names) == 0 {
		return
	}
	budget := h.sandboxRecordBudget
	if budget <= 0 {
		budget = sandboxRecordsPerTick
	}
	now := h.d.clk().Now()
	n := len(names)
	cursor := h.d.sandboxRecordCursor
	start := sort.Search(n, func(i int) bool { return names[i] > cursor }) % n
	checked := 0
	for i := 0; i < n && checked < budget; i++ {
		name := names[(start+i)%n]
		h.d.sandboxRecordCursor = name
		rec, ok, err := session.ReadSandboxRecord(h.town(), name)
		if err != nil || !ok {
			continue
		}
		if !rec.CreatedAt.IsZero() && now.Sub(rec.CreatedAt) < sandboxRecordGrace {
			continue
		}
		checked++
		// A tmux error is no proof the session is gone, so the record is kept
		// and judged again next tick: the same fail-closed rule the landing
		// path follows (gt-0vb2fy).
		live, err := h.sandboxSeatSessionLive(name)
		if err != nil {
			h.d.logger.Printf("patrol_scan: sandbox record %s kept: cannot ask tmux whether the session is live: %v", name, err)
			continue
		}
		if live {
			continue
		}
		exists, err := h.sandboxStillExists(rec.Gateway, name)
		if err != nil {
			h.countGatewayTry(rec)
			h.d.logger.Printf("patrol_scan: sandbox record %s kept: cannot ask gateway %q: %v", name, rec.Gateway, err)
			continue
		}
		if exists {
			// A gateway that cannot give the phase is silent for this tick too
			// (gt-7h38pd). A Stopped sandbox cannot answer the ended-marker
			// question, so it is ended on its phase and seat, not its marker.
			phase, err := h.sandboxPhase(rec.Gateway, name)
			if err != nil {
				h.countGatewayTry(rec)
				h.d.logger.Printf("patrol_scan: sandbox record %s kept: cannot ask gateway %q for its phase: %v", name, rec.Gateway, err)
				continue
			}
			// Both questions were answered, so the gateway is no longer silent.
			h.d.clearGatewayTally(rec.Rig, rec.Polecat)
			end := h.endHeldSandbox
			if isStoppedPhase(phase) {
				end = h.endStoppedSandbox
			}
			dropped, why := h.deleteHeldSandbox(name, rec, end)
			if why != "" {
				h.settleGatewayKeep(rec, why)
				h.d.logger.Printf("patrol_scan: sandbox record %s kept: %s (%s/%s)", name, why, rec.Rig, rec.Polecat)
				continue
			}
			if !dropped {
				continue // the landing removed the record first: nothing left here
			}
			h.completeKeptSeatEnd(rec)
			continue
		}
		// A nil deleter drops the record and the routed client and leaves the
		// sandbox alone: there is none. The gateway answered that it has no
		// sandbox, so it is no longer silent.
		h.d.clearGatewayTally(rec.Rig, rec.Polecat)
		if err := h.removeRecord(name); err != nil {
			h.d.logger.Printf("patrol_scan: sandbox record %s: removing it failed: %v", name, err)
			continue
		}
		h.d.logger.Printf("patrol_scan: sandbox record %s removed: the gateway has no such sandbox (%s/%s)", name, rec.Rig, rec.Polecat)
		h.completeKeptSeatEnd(rec)
	}
}

// completeKeptSeatEnd offers the end of a seat the end kept once the patrol has
// deleted the sandbox that kept it and removed its record (gt-cz5tdz). The end
// runs again through Daemon.sandboxSeatEnder with the landing facts the kept
// record holds, so it measures the sandbox gone and every other guard afresh:
// a seat whose work is unlanded or unpushed stays kept, with that reason
// rewritten into its record, and only a seat every guard lets go is released.
// The patrol never clears the record or releases the seat itself.
//
// A record without landing facts, one that cannot be read, and an end that does
// not complete are each logged with the seat's name, and the keep stands: the
// pass goes on to the next record. The two gateway questions the end asks are
// outside the walk's record budget (see patrolScanSandboxRecords).
func (h *patrolScanHost) completeKeptSeatEnd(rec session.SandboxRecord) {
	if rec.Rig == "" || rec.Polecat == "" {
		return // a record that names no seat has no kept record to complete
	}
	path, err := keptSandboxSeatPath(h.town(), rec.Rig, rec.Polecat)
	if err != nil {
		h.d.logger.Printf("patrol_scan: sandbox record %s: no kept record of %s/%s can be read: %v", rec.Session, rec.Rig, rec.Polecat, err)
		return
	}
	kept, err := readKeptSandboxSeatRecord(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return // the seat was not kept
	case err != nil:
		h.d.logger.Printf("patrol_scan: sandbox seat end %s/%s: its kept record is unreadable, so its seat end is not completed; the keep stands: %v", rec.Rig, rec.Polecat, err)
		return
	case kept.Landing == nil:
		h.d.logger.Printf("patrol_scan: sandbox seat end %s/%s: its kept record names no landing facts, so its seat end cannot be completed; the keep stands (%s)", rec.Rig, rec.Polecat, kept.Reason)
		return
	}
	if !h.d.sandboxSeatEnder()(rec.Rig, rec.Polecat, *kept.Landing) {
		h.d.logger.Printf("patrol_scan: sandbox seat end %s/%s: its seat end did not complete after its sandbox was deleted; the keep stands (%s)", rec.Rig, rec.Polecat, kept.Reason)
	}
}

// sandboxSeatSessionLive asks tmux whether a session answers to the sandbox's
// name. The routed tmux client asks the sandbox itself, so a running sandboxed
// seat answers live, and a set ended marker alone does not prove that session
// has ended. It returns the tmux error rather than a verdict: a caller decides
// what an unknown answer means (gt-0vb2fy).
func (h *patrolScanHost) sandboxSeatSessionLive(name string) (bool, error) {
	return h.d.tmux.HasSession(name)
}

// deleteHeldSandbox deletes a held sandbox the patrol's pass found, under the
// seat lock the landing's own end takes, so a landing that removed the record
// first is seen and neither path deletes the sandbox twice (gt-0vb2fy). It
// re-reads the record under the lock. It reports whether the record is gone
// (true: the patrol completes the seat's end) and, when the sandbox is kept,
// why. A record removed between the pass's read and this lock is a quiet skip:
// not kept, and no deleter is called. end decides whether the sandbox goes:
// endHeldSandbox for a running sandbox, endStoppedSandbox for a Stopped one.
func (h *patrolScanHost) deleteHeldSandbox(name string, rec session.SandboxRecord, end func(caller, name string, rec session.SandboxRecord) string) (dropped bool, why string) {
	if err := polecat.CheckSandboxSeatNames(rec.Rig, rec.Polecat); err != nil {
		return false, err.Error()
	}
	err := h.withSeatLock(rec.Rig, rec.Polecat, func() error {
		current, ok, err := session.ReadSandboxRecord(h.town(), name)
		if err != nil {
			why = "cannot re-read its sandbox record under its seat lock: " + err.Error()
			return nil
		}
		if !ok {
			return nil // the landing removed it first: nothing to delete here
		}
		if why = end("patrol_scan", name, current); why != "" {
			return nil
		}
		if err := h.removeRecord(name); err != nil {
			why = "its sandbox is deleted but its record could not be removed: " + err.Error()
			return nil
		}
		dropped = true
		return nil
	})
	if err != nil {
		return false, "its seat lock could not be taken: " + err.Error()
	}
	return dropped, why
}

// withSeatLock runs fn under the seat lock of rig/name: the sandboxSeatLock seam
// when set, else polecat.WithSandboxSeatLock. A lock that is not free in time is
// an error, and fn does not run.
func (h *patrolScanHost) withSeatLock(rig, name string, fn func() error) error {
	lock := h.sandboxSeatLock
	if lock == nil {
		lock = polecat.WithSandboxSeatLock
	}
	return lock(filepath.Join(h.town(), rig), name, fn)
}

// endHeldSandbox ends a held sandbox the gateway still has, whose seat has
// ended: the sandbox answers whether its ended marker is set, and when it is,
// the record deleter copies the seat's transcripts and deletes the sandbox and
// its seat token. It returns the reason the sandbox and its record are kept, or
// "" when the sandbox is deleted. Every answer short of a set marker, every
// failure, and a binary with no deleter linked keep the sandbox and its record,
// so a seat still starting or crashed is never deleted here. caller labels the
// delete it logs, since both the record pass and the landing's end call it.
func (h *patrolScanHost) endHeldSandbox(caller, name string, rec session.SandboxRecord) string {
	ended, err := h.sandboxEndedMarkerSet(rec.Gateway, name)
	if err != nil {
		return "cannot ask its sandbox for its ended marker: " + err.Error()
	}
	if !ended {
		return "its ended marker is not set"
	}
	deleter := h.recordDeleter()
	if deleter == nil {
		// Dropping the record without a deleter would leave the sandbox behind
		// with nothing left to name it, so it is kept for the operator.
		return "no sandbox deleter is linked, so its ended sandbox cannot be deleted"
	}
	if err := deleter(h.town(), rec); err != nil {
		return "its ended sandbox could not be deleted: " + err.Error()
	}
	h.d.logger.Printf("%s: sandbox record %s deleted: its seat ended and the sandbox's ended marker is set; its transcripts are copied before the delete, and a failed copy is warned (%s/%s)", caller, name, rec.Rig, rec.Polecat)
	return ""
}

// endStoppedSandbox ends a held sandbox the gateway reports Stopped, with no
// ended-marker read: a Stopped sandbox cannot answer that question, so the
// marker never decides it. The sandbox is deleted through the record deleter
// only when the seat's end is recorded with landing facts, the landing record
// proves the work landed, and the seat still holds that landed bead and branch
// (staleStoppedKeep). The transcripts may not be copied out of a stopped
// sandbox, so the delete is warned, naming the seat, and goes on. A seat whose
// work is not proven landed stays, kept with stoppedUnlandedReason. The agent
// bead's ended state is deliberately not an alternative: only a kept record with
// landing facts, bound to the seat's current bead and branch, authorizes the
// delete. It returns the reason the sandbox is kept, or "" when it was deleted
// (gt-7h38pd).
func (h *patrolScanHost) endStoppedSandbox(caller, name string, rec session.SandboxRecord) string {
	if rec.Rig == "" || rec.Polecat == "" {
		return "its record names no seat, so its seat's end cannot be read"
	}
	seat := rec.Rig + "/" + rec.Polecat
	kept, ok, err := h.readKeptSeatFor(rec.Rig, rec.Polecat)
	switch {
	case err != nil:
		return "its kept record cannot be read: " + err.Error()
	case !ok:
		return "its seat's end is not recorded"
	case kept.Landing == nil:
		return "its kept record names no landing facts"
	case !kept.Landing.Landed:
		h.recordKeptSeat(rec.Rig, rec.Polecat, *kept.Landing, stoppedUnlandedReason)
		return stoppedUnlandedReason
	}
	if stale := h.staleStoppedKeep(rec.Rig, rec.Polecat, *kept.Landing); stale != "" {
		return stale
	}
	deleter := h.recordDeleter()
	if deleter == nil {
		return "no sandbox deleter is linked, so its stopped sandbox cannot be deleted"
	}
	h.d.logger.Printf("%s: warning: sandbox record %s is Stopped, so its transcripts may not be copied out before the delete (%s)", caller, name, seat)
	if err := deleter(h.town(), rec); err != nil {
		return "its stopped sandbox could not be deleted: " + err.Error()
	}
	h.d.logger.Printf("%s: sandbox record %s deleted: it is Stopped and its seat's work landed (%s)", caller, name, seat)
	return ""
}

// staleStoppedKeep checks that a seat's landing facts still describe the seat
// before a Stopped sandbox is deleted on them. The landing proved the bead and
// branch it names, and a seat slung again since then holds another bead or
// branch: its hook names a different bead, or its branch is not the landed one.
// A seat whose hook is clear and whose branch is the landed one passes, which is
// what gt done leaves after a landing. A mismatch keeps the sandbox, records the
// keep with sandboxKeptRecordStaleReason, and returns it. A seat whose agent bead
// cannot be read is kept with the read's error and nothing recorded. It returns
// "" when the facts describe the seat (gt-7h38pd).
//
// A same-bead sling with the same branch name is not told apart here: the agent
// bead carries no sling time, and the sling helper the landing path uses
// (land.SlungAtIn) answers per bead, not per seat.
func (h *patrolScanHost) staleStoppedKeep(rig, name string, landing sandboxSeatLanding) string {
	fields, err := h.seatAgentFields(rig, name)
	switch {
	case err != nil:
		return "cannot read its agent bead: " + err.Error()
	case fields == nil:
		return "its agent bead is gone"
	}
	hook := strings.TrimSpace(fields.HookBead)
	branch := strings.TrimSpace(fields.Branch)
	landedBranch := strings.TrimSpace(landing.Branch)
	if (hook == "" || hook == landing.BeadID) && landedBranch != "" && branch == landedBranch {
		return ""
	}
	reason := fmt.Sprintf(sandboxKeptRecordStaleReason,
		orMissingText(landing.BeadID), orMissingText(landedBranch), orMissingText(hook), orMissingText(branch))
	h.recordKeptSeat(rig, name, landing, reason)
	return reason
}

// recordDeleter is the record deleter a held sandbox is deleted through: the
// seam when one is set, else session.SandboxDeleter. It is nil when the binary
// linked no deleter and the unit tier has said so.
func (h *patrolScanHost) recordDeleter() func(townRoot string, rec session.SandboxRecord) error {
	if h.sandboxDeleter != nil {
		return h.sandboxDeleter
	}
	if h.noSandboxDeleterLinked {
		return nil
	}
	return session.SandboxDeleter
}

// sandboxPhase asks the gateway for the phase of the sandbox called name: the
// sandboxStatus seam when set, else session.GatewaySandboxPhase. A gateway that
// cannot answer is an error, never a phase.
func (h *patrolScanHost) sandboxPhase(gateway, name string) (string, error) {
	status := h.sandboxStatus
	if status == nil {
		status = session.GatewaySandboxPhase
	}
	st, err := status(gateway, name)
	if err != nil {
		return "", err
	}
	return st.Phase, nil
}

// sandboxEndedMarkerSet asks the sandbox called name on gateway whether its
// ended marker is set: session.SandboxEndedMarkerPresent when no seam is set.
func (h *patrolScanHost) sandboxEndedMarkerSet(gateway, name string) (bool, error) {
	if h.sandboxEndedMarker != nil {
		return h.sandboxEndedMarker(gateway, name)
	}
	return session.SandboxEndedMarkerPresent(gateway, name)
}

// removeRecord drops a sandbox's registry record and routed client.
func (h *patrolScanHost) removeRecord(name string) error {
	if h.removeSandboxRecord != nil {
		return h.removeSandboxRecord(h.town(), name)
	}
	return session.CleanupSandboxWith(nil, h.town(), name)
}
