package land

import "strings"

// This file is what a re-queue reads off a rejected bead: the rejected head,
// and whether that head is still the one the branch carries. gt land requeue
// uses both to decide whether an unchanged landing can go back in the queue
// (gt-3e1z4), and gt done uses the head to tell a polecat it has nothing new
// to submit.

// RejectedHead is the head a work bead's live rejection names: the tip the
// landing worker refused, recorded on the rejection's Head: line. It is the
// commit gt done compares a resubmission against and the commit gt land
// requeue re-queues. ok is false when the notes hold no MERGE REJECTION block,
// or the block names no head (a rejection recorded before Land wrote Head:).
func RejectedHead(notes string) (string, bool) {
	n, ok := ParseRejectionNote(notes)
	if !ok {
		return "", false
	}
	head := strings.TrimSpace(n.Head)
	if head == "" {
		return "", false
	}
	return head, true
}

// HeadsEqual reports whether a and b name the same commit. Both are trimmed and
// compared case-insensitively; an abbreviated form (at least 7 hex digits) also
// matches the full form it prefixes, because a note may record either.
func HeadsEqual(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(a) >= 7 && strings.HasPrefix(b, a) {
		return true
	}
	return len(b) >= 7 && strings.HasPrefix(a, b)
}
