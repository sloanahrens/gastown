package land

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// omReusedSuffix is the stage-line suffix for a landing that reused a stored om
// verdict instead of running om: cost is what om took when it ran, and age is
// how long ago its verdict was stored. It has the shape of the
// ", om skipped (overseer-reviewed)" suffix. Both durations print through
// reusedDuration. A negative age prints with its sign rather than being
// clamped to zero, so a bad input shows.
func omReusedSuffix(cost, age time.Duration) string {
	return fmt.Sprintf(", om reused (%s, %s old)", reusedDuration(cost), reusedDuration(age))
}

// reusedDuration prints a reuse duration as stageLine prints om's time:
// Duration.Round(time.Second). omReusedSuffix and the bead comment both print
// through it, so the stage line and the bead read one format for the same age.
func reusedDuration(d time.Duration) string {
	return d.Round(time.Second).String()
}

// commentReused writes the review record a landing that reused a stored om
// verdict leaves on its work bead, in place of commentVerdict's: the verdict
// and its score, the merged head the landing records, and the reviewed base and
// how long ago the verdict was stored (gt-eyzgl6). The head marker and the
// verdict word are the ones commentVerdict writes, so the verdict-comment dedupe
// recognizes this comment, and a second call writes nothing.
//
// Like commentVerdict it is bounded and fail-soft: the write runs under the
// acceptedRiskTimeout budget, a write that fails or outlasts it is queued for a
// later pass, and nothing it does changes the landing. A record without a base
// or review time, or a landing without a merged head, is refused and logged,
// because a comment built from it would name a head, a base or an age that is
// not there.
func (l *Lander) commentReused(w Work, v Verdict, rec AheadRecord) {
	if w.CandidateHead == "" || rec.Base == "" || rec.ReviewedAt.IsZero() {
		l.logf("%s: the reused om verdict is not commented: the merged head, the reviewed base or the review time is missing", w.BeadID)
		return
	}
	head, prefix := verdictCommentHead(w.CandidateHead), "om "+verdictCommentWord(v)+" "
	text := fmt.Sprintf("%s%.2f %s (reused from %s, %s old)", prefix, v.Score, head,
		shortSHA(rec.Base), reusedDuration(l.now().Sub(rec.ReviewedAt)))
	budget := l.acceptedRiskTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	done := make(chan commentOutcome, 1)
	go func() { done <- l.writeComment(w.BeadID, text, head, prefix) }()
	select {
	case out := <-done:
		switch {
		case out.writeErr != nil:
			l.logf("%s: could not comment the reused om verdict on the bead (%v); the landing is unaffected", w.BeadID, out.writeErr)
			l.queueVerdictComment(w.BeadID, text, w.CandidateHead)
		case !out.wrote:
			l.logf("%s: this landing attempt's om verdict is already on the bead; no second comment", w.BeadID)
		case out.readErr != nil:
			l.logf("%s: could not read the bead's comments to check for this landing's om verdict (%v); the comment was written anyway", w.BeadID, out.readErr)
		}
	case <-ctx.Done():
		l.logf("%s: commenting the reused om verdict on the bead did not finish in %s; the landing is unaffected", w.BeadID, budget)
		l.queueVerdictComment(w.BeadID, text, w.CandidateHead)
	}
}

// applyBar applies the rig's bar to v: an approve scored below bar becomes a
// request for changes, with the summary prefix reviewOnce writes. Any other
// verdict is returned unchanged. reviewOnce keeps its own copy of this rule,
// and TestVerdictReuseApplyBarMatchesReviewOnce pins the two together, so a
// reused verdict is judged the way a fresh one is.
func applyBar(v Verdict, bar float64) Verdict {
	if v.Verdict == VerdictApprove && v.Score < bar {
		v.Verdict = VerdictRequestChanges
		v.Summary = strings.TrimSpace(fmt.Sprintf("score %.2f is below the rig's .om.json threshold %.2f. %s", v.Score, bar, v.Summary))
	}
	return v
}
