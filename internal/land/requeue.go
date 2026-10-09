package land

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// This file is what a re-queue writes to, and reads off, a rejected bead: the
// rejected head and whether that head is still the one the branch carries. gt
// land requeue uses both to decide whether an unchanged landing can go back in
// the queue (gt-3e1z4), and gt done uses the head to tell a polecat it has
// nothing new to submit.

// RequeueCommentMarker opens the comment gt land requeue records on a bead it
// puts back in the queue. Its Attempt: line names the rejection it re-queues,
// which is what tells a requeued block from one an interrupted pass left, both
// of which leave the bead ready with the refusal label gone (gt-en9gs).
const RequeueCommentMarker = "REQUEUED:"

// FormatRequeueComment renders gt land requeue's comment: who re-queued the
// landing, on what head, and the attempt number of the rejection it undoes.
func FormatRequeueComment(actor, branch, head string, attempt int, reason string) string {
	return fmt.Sprintf("%s %s re-queued this rejected landing unchanged\nBranch: %s\nHead: %s\nAttempt: %d\nReason: %s",
		RequeueCommentMarker, actor, branch, head, attempt, reason)
}

// RequeuedAttempt is the attempt number of the newest REQUEUED comment on a
// bead, the rejection that comment undid. ok is false when no comment records a
// re-queue, or the newest one names no attempt (written before the line
// existed).
func RequeuedAttempt(comments []beads.Comment) (int, bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		text := comments[i].Text
		if !strings.HasPrefix(strings.TrimSpace(text), RequeueCommentMarker) {
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			value, found := strings.CutPrefix(strings.TrimSpace(line), "Attempt:")
			if !found {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				return n, true
			}
			return 0, false
		}
		return 0, false
	}
	return 0, false
}

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
