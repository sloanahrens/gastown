package slot

// HoldResolution is the live pool's verdict on a ring entry whose hold was
// never closed. See ResolveHolds for why an open entry needs one.
type HoldResolution string

const (
	// HoldLive: the pool holds the entry's slot and its owner file names the
	// entry's holder — the hold on record is the hold in progress.
	HoldLive HoldResolution = "live"

	// HoldAbandoned: the pool no longer counts the entry's holder, because the
	// slot is free or a different holder has taken it. That hold ended without
	// the Release that writes held_s, so a holder killed outright and one whose
	// release was lost read the same here; in both cases the slot is nobody's.
	HoldAbandoned HoldResolution = "abandoned"

	// HoldUnmatched: the pool holds the entry's slot but names no holder for it
	// (the display-only owner file was never written, or is unreadable), so
	// nothing can say whether this entry is that holder.
	HoldUnmatched HoldResolution = "unmatched"
)

// ResolvedHold is a ring entry paired with the pool's verdict on a hold the
// entry left open.
type ResolvedHold struct {
	HistoryEntry
	// Resolution is how an entry with no recorded release reads against the
	// pool, and is empty for an entry whose own fields already say: held_s is a
	// release, timed_out a caller that never got the slot.
	Resolution HoldResolution `json:"resolution,omitempty"`
	// ReclaimedBy is the holder that has since taken the entry's slot, when
	// Resolution is HoldAbandoned and the slot was re-granted rather than left
	// free.
	ReclaimedBy *Owner `json:"reclaimed_by,omitempty"`
}

// ResolveHolds pairs every entry with what rep says about a hold that was never
// closed, leaving the rest as they were.
//
// rep must be the report for the slots the entries came from. It is the half
// an open entry cannot carry itself: a grant is written at acquire time and
// only a Release writes held_s, so a hold that ended without one is
// indistinguishable, in the ring file alone, from a hold still in progress —
// which is how a history line came to claim an unreleased holder under a header
// reading 0/5 held (gt-2tqe). The pool is the authority on what is held now,
// and the caller that prints the history prints that same report's held/free
// picture directly above it, so the two cannot disagree.
func ResolveHolds(entries []HistoryEntry, rep Report) []ResolvedHold {
	bySlot := make(map[int]SlotState, len(rep.Slots))
	for _, st := range rep.Slots {
		bySlot[st.Index] = st
	}

	var out []ResolvedHold
	for _, e := range entries {
		rh := ResolvedHold{HistoryEntry: e}
		if e.HeldS == nil && !e.TimedOut {
			st := bySlot[e.Slot]
			switch {
			case !st.Held:
				rh.Resolution = HoldAbandoned
			case st.Owner == nil:
				rh.Resolution = HoldUnmatched
			case st.Owner.PID == e.PID && st.Owner.Role == e.Role:
				rh.Resolution = HoldLive
			default:
				rh.Resolution = HoldAbandoned
				rh.ReclaimedBy = st.Owner
			}
		}
		out = append(out, rh)
	}
	return out
}
