package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specChildrenLease bounds how long a remembered children read stands. The
// dispatcher's children read is one bd call per candidate per tick, and nearly
// every answer repeats the last one; but a child's close and a branch's new
// files do not move the parent's updated_at, so the memory is a shortcut with a
// lease, never the truth (gt-km9jt).
const specChildrenLease = 5 * time.Minute

// specChildrenMemory is one bead's children as a tick read them: the board
// row's updated_at at the read, when the read was made, and the children.
type specChildrenMemory struct {
	UpdatedAt string               `json:"updated_at"`
	ReadAt    time.Time            `json:"read_at"`
	Children  []specdispatch.Child `json:"children"`
}

// children answers beadID's children from the memory when the tick's board row
// carries the updated_at the memory was read at and the read is still inside
// its lease, and runs read otherwise. read is the tick's own read, so the child
// set is the one it would have looked up itself.
func (a *specAnnotated) children(beadID string, read func() ([]specdispatch.Child, error)) ([]specdispatch.Child, error) {
	a.mu.Lock()
	updated, listed := a.rows[beadID]
	entry, remembered := a.candidates[beadID]
	now := a.now()
	a.mu.Unlock()
	if listed && remembered && updated != "" && updated == entry.UpdatedAt &&
		now.Sub(entry.ReadAt) <= specChildrenLease && specChildrenKnown(entry.Children) {
		return append([]specdispatch.Child(nil), entry.Children...), nil
	}
	children, err := read()
	if err != nil {
		return nil, err
	}
	if !listed || updated == "" {
		// No updated_at to compare the next tick's row with, so nothing to
		// remember the read against.
		return children, nil
	}
	a.mu.Lock()
	a.candidates[beadID] = specChildrenMemory{
		UpdatedAt: updated,
		ReadAt:    now,
		Children:  append([]specdispatch.Child(nil), children...),
	}
	a.dirty = true
	a.mu.Unlock()
	return children, nil
}

// specChildrenKnown reports whether a remembered child set is one this code
// wrote. A child with no status is not: OpenChildHold would read it as open, so
// an entry from a schema this code does not know is read again rather than
// answered (gt-km9jt).
func specChildrenKnown(children []specdispatch.Child) bool {
	for _, c := range children {
		if c.ID == "" || c.Status == "" {
			return false
		}
	}
	return true
}
