package landworker

import "time"

// outcomeMemo remembers, per bead, the list row an outcome left it on and when
// the bead is worth reading again. Processing one bead costs a Show, a Children
// and a Comments, and a held bead — a refused submission, a missing branch, a
// failure backing off — stays in the ready list unchanged, so without this the
// worker pays those reads every pass for a row it has already judged
// (gt-r9pj9).
//
// The key is the bead's updated_at from the list row: a label, a comment or a
// status change moves it, so a changed bead is read on the next pass. A change
// that does not touch the bead, its remote branch moving, is what the hold
// covers: the bead is read again once until passes.
type outcomeMemo struct {
	now     func() time.Time
	entries map[string]memoEntry
}

type memoEntry struct {
	updatedAt string
	until     time.Time
}

func newOutcomeMemo(now func() time.Time) *outcomeMemo {
	return &outcomeMemo{now: now, entries: map[string]memoEntry{}}
}

// held reports whether id, as the list row describes it, is a bead the worker
// judged already and need not read again yet. A row with no updated_at cannot
// prove it is unchanged, so it is never held: a bead that might be landable is
// read.
func (m *outcomeMemo) held(id, updatedAt string) bool {
	if m == nil || updatedAt == "" {
		return false
	}
	e, ok := m.entries[id]
	if !ok || e.updatedAt != updatedAt {
		return false
	}
	return m.now().Before(e.until)
}

// record remembers an outcome that left id where it was until until.
func (m *outcomeMemo) record(id, updatedAt string, until time.Time) {
	if m == nil || updatedAt == "" {
		return
	}
	m.entries[id] = memoEntry{updatedAt: updatedAt, until: until}
}

// forget drops id: its outcome was a landing, so nothing about it is held.
func (m *outcomeMemo) forget(id string) {
	if m == nil {
		return
	}
	delete(m.entries, id)
}
