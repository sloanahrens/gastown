package gocacheprog

import (
	"maps"
	"time"
)

// IndexEntry is one action's output, its size and last use. PutTime is the time of
// the put that set OutputID and Size; Merge compares a later put against it.
type IndexEntry struct {
	OutputID string
	Size     int64
	PutTime  time.Time
	LastUse  time.Time
}

// Index maps an action id to its entry.
type Index map[string]IndexEntry

// Merge returns idx with records applied, leaving idx unchanged. All puts apply
// before all uses, so within one call record order and repetition do not matter:
// a use of an action put in the same call is kept, and merging the same records
// again changes nothing.
//
// A use of an action held neither by idx nor by a put of this call is ignored and
// not remembered, so a put in a later call carries its own time as last use. An
// entry never exists without a size, so a caller passes every record it holds in
// one call.
func Merge(idx Index, records []Record) Index {
	out := make(Index, len(idx))
	maps.Copy(out, idx)
	for _, r := range records {
		if r.Op != opPut {
			continue
		}
		e, ok := out[r.ActionID]
		if !ok || putWins(r, e) {
			e.OutputID, e.Size, e.PutTime = r.OutputID, r.Size, r.Time
		}
		if r.Time.After(e.LastUse) {
			e.LastUse = r.Time
		}
		out[r.ActionID] = e
	}
	// A use of an unknown action is dropped: an entry never exists without a size.
	for _, r := range records {
		if e, ok := out[r.ActionID]; ok && r.Op == opUse && r.Time.After(e.LastUse) {
			e.LastUse = r.Time
			out[r.ActionID] = e
		}
	}
	return out
}

// putWins reports whether put r replaces e's output and size: later time, then greater output id, then greater size.
func putWins(r Record, e IndexEntry) bool {
	if !r.Time.Equal(e.PutTime) {
		return r.Time.After(e.PutTime)
	}
	if r.OutputID != e.OutputID {
		return r.OutputID > e.OutputID
	}
	return r.Size > e.Size
}
