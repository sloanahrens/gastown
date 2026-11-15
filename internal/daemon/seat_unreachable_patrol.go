package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// seatReadSet is one polecat-health pass's session reads. The rigs walk on the
// rig pool's goroutines, so adds are locked; the pass hands the set to the
// tracker once every rig has walked (gt-q6hafz).
//
// Every seat the pass listed is in it exactly once: either a read (the session
// check ran) or a skip (the seat was left before the check, so no read
// answered). The two are kept apart because they mean different things to the
// tracker, which reads a seat it is not told about as released.
type seatReadSet struct {
	mu         sync.Mutex
	reads      []seatRead
	skipped    []seatKey
	unreadable bool
}

// add records one seat's session read.
func (s *seatReadSet) add(read seatRead) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, read)
}

// skip records a listed seat whose session check did not run, so the pass has
// no read for it. It is not a release: the watch carries the seat's last read,
// so a running outage neither clears nor starts on a pass that did not check
// the seat (gt-q6hafz).
func (s *seatReadSet) skip(rigName, polecatName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipped = append(s.skipped, seatKey{rig: rigName, polecat: polecatName})
}

// unlisted records that a rig's seat list could not be read. The pass then says
// nothing about any seat rather than reading the seats it could not list as
// released.
func (s *seatReadSet) unlisted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unreadable = true
}

// list returns the pass's reads. The copy keeps the caller's append off the
// set's own store.
func (s *seatReadSet) list() []seatRead {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seatRead(nil), s.reads...)
}

// skips returns the seats the pass listed and did not check.
func (s *seatReadSet) skips() []seatKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seatKey(nil), s.skipped...)
}

// listed reports whether every rig's seat list was read.
func (s *seatReadSet) listed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.unreadable
}

// seatUnreachableWatch keeps the tracker's first-failure times from one
// polecat-health pass to the next, and the items the attention collector raises
// from its alerts.
type seatUnreachableWatch struct {
	mu      sync.Mutex
	outages map[seatKey]seatOutage
	items   []attention.Item
}

// observe hands one pass's reads to the tracker at now and replaces the items it
// raises.
//
// A seat the pass listed but did not check has no read behind it: the intent
// record said held, the work was submitted, or the record could not be read.
// The tracker drops a seat it is not given, which is how a released seat stops
// alarming, so handing it nothing would clear a running outage with no read
// that cleared it (gt-q6hafz). The seat's last read is handed over in its
// place, so the tracker keeps the first-failure time and names the bead and
// error the check last returned. A skipped seat with no outage behind it gets
// nothing, so a seat that is paused from the start never raises.
//
// A pass whose seat list could not be read says nothing about any seat: the
// tracker keeps its outages and the items stay as the last readable pass left
// them, so an unanswered list read neither raises a new item nor clears a
// running one.
func (w *seatUnreachableWatch) observe(now time.Time, set *seatReadSet) {
	w.mu.Lock()
	defer w.mu.Unlock()
	reads := set.list()
	// The carry is appended after the reads, so the checked seats are named
	// first and the two kinds never share a key: a skipped seat was not read.
	for _, key := range set.skips() {
		if out, ok := w.outages[key]; ok {
			reads = append(reads, seatRead{Rig: key.rig, Polecat: key.polecat, Bead: out.bead, Err: out.err})
		}
	}
	listed := set.listed()
	next, alerts := trackSeatUnreachable(w.outages, now, reads, listed)
	w.outages = next
	if !listed {
		return
	}
	items := make([]attention.Item, 0, len(alerts))
	for _, alert := range alerts {
		items = append(items, seatUnreachableItem(alert, now))
	}
	w.items = items
}

// observed returns the items the last readable pass raised, for the attention
// collector.
func (w *seatUnreachableWatch) observed() []attention.Item {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]attention.Item(nil), w.items...)
}

// collectSeatUnreachable raises the items the polecat-health pass's tracker
// decided on this heartbeat (gt-q6hafz).
func (s *attentionSources) collectSeatUnreachable(_ context.Context) ([]attention.Item, error) {
	if s.seatUnreachable == nil {
		return nil, nil
	}
	return s.seatUnreachable(), nil
}

// seatUnreachableItem is the queue's item for one seat whose session has not
// answered since alert.Since. Its key is the seat, so it holds one item however
// many passes stay unreadable, and it clears when the tracker stops naming the
// seat.
func seatUnreachableItem(alert seatUnreachable, now time.Time) attention.Item {
	held := alert.Bead
	if held == "" {
		held = "no bead read"
	}
	return attention.Item{
		Key:      "seat-unreachable:" + alert.Rig + "/" + alert.Polecat,
		Kind:     attention.KindSeatUnreachable,
		Severity: attention.SeverityHigh,
		Rig:      alert.Rig,
		Bead:     alert.Bead,
		Summary: fmt.Sprintf("%s/%s unreadable for %s, holding %s: %v",
			alert.Rig, alert.Polecat, parkedAge(now.Sub(alert.Since)), held, alert.Err),
	}
}

// seatHookedBead is the bead a seat holds, for a seat-unreachable item to name:
// the intent record's work bead when a writer set one, otherwise the work bead
// assigned to the seat with status hooked or in_progress. A read that fails
// leaves it "" and is logged: the seat is still named for its unreadable
// session, without a bead the daemon could not read guessed at (gt-q6hafz).
func (d *Daemon) seatHookedBead(rigName, polecatName, recorded string, tm *seatTimer) string {
	if recorded != "" {
		return recorded
	}
	assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
	var bead string
	var err error
	d.timeSeatCall(tm, seatBD, func() { bead, err = d.assignedActiveWorkBead(rigName, assignee) })
	if err != nil {
		d.logger.Printf("Error reading the work bead of %s/%s: %v", rigName, polecatName, err)
	}
	return bead
}
