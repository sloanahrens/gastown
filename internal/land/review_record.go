package land

import (
	"fmt"
	"strings"
)

// NoteAppender is the one write the review recorders make: append a line to a
// bead's notes. beads.Client satisfies it.
type NoteAppender interface {
	AppendNotes(id, note string) error
}

// OverseerReviewLine renders the note line that records verdict for head, the
// form overseerReviewLine reads back: findings ride a FAIL as " findings=a,b",
// and a WAIVED or AUDITED carries its reason after a colon. reason is folded
// to one line, so it cannot start a second note line.
func OverseerReviewLine(head, verdict string, findings []string, reason string) string {
	line := OverseerReviewMarker + " " + head + " " + verdict
	if len(findings) > 0 {
		line += " findings=" + strings.Join(findings, ",")
	}
	if reason = NoteField(reason); reason != "" {
		line += ": " + reason
	}
	return line
}

// ReviewConflictError reports that head already holds a different verdict
// than the one asked for, so nothing was written: the first review of a head
// stands, except that a FAIL is superseded by a later verdict.
type ReviewConflictError struct {
	BeadID, Head, Recorded, Wanted string
}

func (e *ReviewConflictError) Error() string {
	return fmt.Sprintf("%s already records %s %s %s; %s was not recorded", e.BeadID, OverseerReviewMarker, e.Head, e.Recorded, e.Wanted)
}

// RecordOverseerReview appends the review line for head to bead id, judged
// against notes, the bead's notes as last read. It is the one writer of the
// review note, shared by gt bead review and the daemon's resolved-FAIL pass,
// so both write the same line under the same rules:
//
//   - head has no review yet: the line is appended.
//   - head's latest review is a FAIL and verdict is not: the line is appended
//     after it, and the latest line stands (OverseerReviewVerdict).
//   - head's latest review is the verdict asked for: nothing is written and
//     written is false, so recording the same verdict twice writes one line.
//   - head's latest review is another verdict: nothing is written and the
//     error is a *ReviewConflictError.
//
// It only appends: an existing review line is never edited or removed.
func RecordOverseerReview(a NoteAppender, id, notes, head, verdict string, findings []string, reason string) (line string, written bool, err error) {
	line = OverseerReviewLine(head, verdict, findings, reason)
	if recorded, ok := OverseerReviewVerdict(notes, head); ok {
		switch {
		case recorded == verdict:
			return line, false, nil
		case recorded != OverseerReviewFail:
			return line, false, &ReviewConflictError{BeadID: id, Head: head, Recorded: recorded, Wanted: verdict}
		}
	}
	if err := a.AppendNotes(id, line); err != nil {
		return line, false, err
	}
	return line, true, nil
}
