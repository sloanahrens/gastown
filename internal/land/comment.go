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
// candidate head it covered (gt-uebmq), plus both scores when a borderline
// rejection was re-scored (gt-cz07b).
func verdictCommentText(v Verdict, head string, start, end time.Time) string {
	return fmt.Sprintf("om %s %.2f (om %s, %s–%s) %s%s",
		verdictCommentWord(v), v.Score, verdictCommentVersion(v),
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), verdictCommentHead(head),
		verdictCommentRescore(v))
}

// verdictCommentRescore renders the re-score clause of a verdict comment: both
// om scores and which run is which, so a bead's reader can tell an approve om
// reached twice — the second time carrying it over the bar — from one it
// reached the first time (gt-cz07b). The rejected run's score is recorded
// nowhere else when the rerun approves.
func verdictCommentRescore(v Verdict) string {
	if v.Rescore == nil {
		return ""
	}
	return fmt.Sprintf(" (re-scored: %.2f rejected first, %.2f on the rerun)", v.Rescore.First, v.Rescore.Rerun)
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

// commentReviewLogFailure writes on the work bead that its om verdict is not
// in the reviews log, so the rework table's undercount traces to the verdict it
// lacks rather than to a daemon log line alone. It is fail-soft and bounded
// like commentVerdict: it never returns an error and never changes the landing.
func (l *Lander) commentReviewLogFailure(w Work, rec ReviewRecord, appendErr error) {
	text := fmt.Sprintf("om %s %.2f (attempt %d) is not in the reviews log: %s",
		rec.Verdict, rec.Score, rec.Attempt, elideMiddle(NoteField(appendErr.Error()), reviewErrorReasonMax))
	done := make(chan error, 1)
	go func() { done <- l.Beads.AddComment(w.BeadID, text) }()
	select {
	case err := <-done:
		if err != nil {
			l.logf("%s: could not comment the failed reviews-log append on the bead (%v); the landing is unaffected", w.BeadID, err)
		}
	case <-time.After(commentTimeout):
		l.logf("%s: commenting the failed reviews-log append on the bead did not finish in %s; the landing is unaffected", w.BeadID, commentTimeout)
	}
}
