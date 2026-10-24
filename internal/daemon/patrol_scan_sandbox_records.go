package daemon

import (
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
// At most the budget (sandboxRecordsPerTick) of records is checked per tick,
// live ones included, so a tick makes at most that many tmux probes and gateway
// questions, and records the gateway still has cannot starve the ones behind
// them. The walk over the name-sorted records starts after the name the last
// tick ended on (Daemon.sandboxRecordCursor), so consecutive ticks judge
// consecutive names and every record is judged within ceil(n/budget) ticks,
// whatever the tick's clock phase. A failed removal is logged and the pass goes
// on.
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
		if exists {
			continue
		}
		// A nil deleter drops the record and the routed client and leaves the
		// sandbox alone: there is none.
		if err := h.removeRecord(name); err != nil {
			h.d.logger.Printf("patrol_scan: sandbox record %s: removing it failed: %v", name, err)
			continue
		}
		h.d.logger.Printf("patrol_scan: sandbox record %s removed: the gateway has no such sandbox (%s/%s)", name, rec.Rig, rec.Polecat)
	}
}

// removeRecord drops a sandbox's registry record and routed client.
func (h *patrolScanHost) removeRecord(name string) error {
	if h.removeSandboxRecord != nil {
		return h.removeSandboxRecord(h.town(), name)
	}
	return session.CleanupSandboxWith(nil, h.town(), name)
}
