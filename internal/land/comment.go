package land

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// commentTimeout bounds the verdict comment — the read that checks for one
// this attempt already wrote and the write itself. A landing the gate and the
// review already decided must not wait on a bead write, so a Dolt that is slow
// or wedged costs a log line and nothing else (gt-uebmq).
const commentTimeout = 10 * time.Second

// commentVerdict writes this landing's review record on the work bead: one
// line naming what the review decided, which om decided it and the candidate
// head it covered, so a bead can be tied to its review without the landing log
// (gt-uebmq). A rejection's own note and findings stay reject's.
//
// The write is fail-soft: it never returns an error and never changes the
// verdict, and everything it logs is logged here rather than in the goroutine
// the timeout needs.
func (l *Lander) commentVerdict(w Work, v Verdict, start, end time.Time) {
	text := verdictCommentText(v, w.CandidateHead, start, end)
	head, prefix := verdictCommentHead(w.CandidateHead), "om "+verdictCommentWord(v)+" "
	done := make(chan commentOutcome, 1)
	go func() { done <- l.writeComment(w.BeadID, text, head, prefix) }()
	select {
	case out := <-done:
		switch {
		case out.writeErr != nil:
			l.logf("%s: could not comment the om verdict on the bead (%v); the landing is unaffected", w.BeadID, out.writeErr)
		case !out.wrote:
			l.logf("%s: this landing attempt's om verdict is already on the bead; no second comment", w.BeadID)
		case out.readErr != nil:
			l.logf("%s: could not read the bead's comments to check for this landing's om verdict (%v); the comment was written anyway", w.BeadID, out.readErr)
		}
	case <-time.After(commentTimeout):
		l.logf("%s: commenting the om verdict on the bead did not finish in %s; the landing is unaffected", w.BeadID, commentTimeout)
	}
}

// commentOutcome is what the comment goroutine reports back: whether the
// comment was written, and the read or write that failed on the way.
type commentOutcome struct {
	wrote    bool
	readErr  error
	writeErr error
}

// writeComment adds text to id unless the bead already carries this landing
// attempt's verdict, named by the head marker and the verdict the text opens
// with. The read is a duplicate guard rather than a precondition, so a bead
// whose comments cannot be read still gets the comment; the worst that costs
// is a second line on a retry.
func (l *Lander) writeComment(id, text, head, prefix string) commentOutcome {
	comments, readErr := l.Beads.Comments(id)
	if readErr == nil && verdictCommented(comments, head, prefix) {
		return commentOutcome{}
	}
	if err := l.Beads.AddComment(id, text); err != nil {
		return commentOutcome{readErr: readErr, writeErr: err}
	}
	return commentOutcome{wrote: true, readErr: readErr}
}

// verdictCommentText is the line a review leaves on the work bead: the verdict
// and its score, the om behind it, the window the review ran in and the
// candidate head it covered (gt-uebmq).
func verdictCommentText(v Verdict, head string, start, end time.Time) string {
	return fmt.Sprintf("om %s %.2f (om %s, %s–%s) %s",
		verdictCommentWord(v), v.Score, verdictCommentVersion(v),
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), verdictCommentHead(head))
}

// verdictCommentWord is the verdict as the comment names it: a landing the
// overseer reviewed in om's place has no om verdict to name, so the waiver
// stands where one would.
func verdictCommentWord(v Verdict) string {
	if strings.HasPrefix(v.Verdict, VerdictOverseerPrefix) {
		return VerdictSkipped + " (overseer-reviewed)"
	}
	return v.Verdict
}

// verdictCommentVersion names the om behind the verdict, and is what
// omVersionUnknown says when the reviewer could not read one.
func verdictCommentVersion(v Verdict) string {
	if v.Version == "" {
		return omVersionUnknown
	}
	return v.Version
}

// verdictCommentHead is the marker in the text that ties a comment to one
// landing attempt.
func verdictCommentHead(head string) string {
	return "[head " + shortSHA(head) + "]"
}

// verdictCommented reports whether comments already carry this landing
// attempt's verdict. The window in the text is not part of the key — a retry
// renders it from its own clock — so the head marker and the verdict are.
func verdictCommented(comments []beads.Comment, head, prefix string) bool {
	for _, c := range comments {
		text := strings.TrimSpace(c.Text)
		if strings.HasPrefix(text, prefix) && strings.Contains(text, head) {
			return true
		}
	}
	return false
}
