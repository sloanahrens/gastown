package daemon

import (
	"errors"
	"os"
	"sort"
	"time"

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
// ended here only when the sandbox answers that the marker is set, and then
// through the record deleter, which copies the seat's transcripts before it
// deletes the sandbox. A marker that is absent, an exec that fails, a deleter
// that fails, and a town with no deleter linked each keep the record, so a seat
// still starting is never deleted by this path. A finish whose stop deletes the
// sandbox removes the record too, so by then there is no record for this path to
// find.
//
// At most the budget (sandboxRecordsPerTick) of records is checked per tick,
// live ones included, so the walk makes at most that many tmux probes and
// gateway questions, and records the gateway still has cannot starve the ones
// behind them. The walk over the name-sorted records starts after the name the
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
		live, err := h.d.tmux.HasSession(name)
		if err != nil {
			h.d.logger.Printf("patrol_scan: sandbox record %s: cannot ask tmux, treating it as not live and asking the gateway: %v", name, err)
		} else if live {
			continue
		}
		exists, err := h.sandboxStillExists(rec.Gateway, name)
		if err != nil {
			h.d.logger.Printf("patrol_scan: sandbox record %s kept: cannot ask gateway %q: %v", name, rec.Gateway, err)
			continue
		}
		if exists && !h.endHeldSandbox(name, rec) {
			continue
		}
		// A nil deleter drops the record and the routed client and leaves the
		// sandbox alone: there is none.
		if err := h.removeRecord(name); err != nil {
			h.d.logger.Printf("patrol_scan: sandbox record %s: removing it failed: %v", name, err)
			continue
		}
		if !exists {
			h.d.logger.Printf("patrol_scan: sandbox record %s removed: the gateway has no such sandbox (%s/%s)", name, rec.Rig, rec.Polecat)
		}
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
// pass goes on to the next record.
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

// endHeldSandbox ends a held sandbox the gateway still has, whose seat has
// ended: the sandbox answers whether its ended marker is set, and when it is,
// the record deleter copies the seat's transcripts and deletes the sandbox and
// its seat token. It reports whether the record may now be dropped. Every answer
// short of a set marker, every failure, and a binary with no deleter linked keep
// the sandbox and its record, so a seat still starting or crashed is never
// deleted here.
func (h *patrolScanHost) endHeldSandbox(name string, rec session.SandboxRecord) bool {
	ended, err := h.sandboxEndedMarkerSet(rec.Gateway, name)
	if err != nil {
		h.d.logger.Printf("patrol_scan: sandbox record %s kept: cannot ask sandbox for its ended marker: %v", name, err)
		return false
	}
	if !ended {
		return false
	}
	deleter := h.sandboxDeleter
	if deleter == nil && !h.noSandboxDeleterLinked {
		deleter = session.SandboxDeleter
	}
	if deleter == nil {
		// Dropping the record without a deleter would leave the sandbox behind
		// with nothing left to name it, so it is kept for the operator.
		h.d.logger.Printf("patrol_scan: sandbox record %s kept: no sandbox deleter is linked, so its ended sandbox cannot be deleted (%s/%s)", name, rec.Rig, rec.Polecat)
		return false
	}
	if err := deleter(h.town(), rec); err != nil {
		h.d.logger.Printf("patrol_scan: sandbox record %s kept: its ended sandbox could not be deleted: %v", name, err)
		return false
	}
	h.d.logger.Printf("patrol_scan: sandbox record %s deleted: its seat ended and the sandbox's ended marker is set; its transcripts are copied before the delete, and a failed copy is warned (%s/%s)", name, rec.Rig, rec.Polecat)
	return true
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
