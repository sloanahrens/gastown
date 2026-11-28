package land

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// reviewCar runs the om review for a car's merge candidate and refuses the
// landing when the review rejects it or reaches no verdict. It returns the
// verdict for the gate and the finish.
func (l *Lander) reviewCar(ctx context.Context, c *car) (reviewed, error) {
	w, issue, g, wt, dir, base, merged := c.w, c.issue, c.g, c.wt, c.dir, c.base, c.merged
	var (
		verdict   Verdict
		reviewErr error
		comments  []beads.Comment
		omTime    time.Duration
		omRetried bool
	)
	// reusedSuffix is the ", om reused (...)" stage suffix when the om stage
	// reused a stored review; it is empty when om ran or the head was waived.
	var reusedSuffix string
	// reusedRec is the stored record a reused om verdict came from; it is nil
	// when om ran or the head was waived. A record commentReused cannot use is
	// commented the way a nil one is (gt-bydaep).
	var reusedRec *AheadRecord
	// reviewStart opens the window the review leaves on the work bead: it
	// covers the overseer check and the om attempt alike, so a waiver is
	// timed like a review (gt-uebmq).
	reviewStart := time.Now()
	waived := OverseerReviewed(issue, w.Head)
	if w.StoredOnly && (waived || l.Ahead == nil) {
		// A stored-only pass consumes a stored verdict and nothing else: a waived
		// head needs no review and no store leaves nothing to consume, so both end
		// it here, before the om config refusal and the reuse lookup (gt-6r4exl).
		return reviewed{}, ErrNoStoredVerdict
	}
	if waived {
		// The overseer reviewed this exact head in place of om, which could
		// not return a verdict (gt-g8t3m): record that and do not pay for om
		// again. The gate below still runs.
		verdict = Verdict{Verdict: VerdictOverseerPrefix + shortSHA(w.Head)}
	} else {
		// A landing that edits the root .om.json changes the bar om judges
		// the head against, so its head waits for the overseer before om runs
		// (gt-93hf4). A waived head skipped this branch above.
		if rej := l.omConfigRejection(g, base, merged, w.Head, w.BeadID); rej != nil {
			return reviewed{}, l.reject(ctx, issue, w, rej, nil)
		}
		l.stage(w, StageOM)
		omCtx, omDone := l.Slow.watch(ctx, l, w, dir, StageOM)
		// The input is assembled by the helper ReviewAhead calls too, so the
		// pool's review is made for the input this stage would hand om
		// (gt-ff359z). A lenient read never fails: the read it could not make
		// is logged and the review still judges the diff.
		in, readComments, used, err := l.reviewInputFor(issue, w, base, merged, dir, g, false)
		if err != nil {
			return reviewed{}, &InfraError{Stage: "review input", Err: err}
		}
		comments = readComments
		// A stored review may stand in for om (gt-er929x). On a hit the verdict is
		// the record's, stamped with the om its key names, and no review runs. A
		// miss, or an answer reuseAhead could not give, runs om as it does with no
		// store; reuseAhead has logged why. An unknown answer is never a hit.
		rec, key, reused, reuseErr := l.reuseAhead(omCtx, g, wt, w, in, base, merged)
		if w.StoredOnly {
			// A stored-only pass runs nothing itself: it consumes a stored
			// rejection and nothing else, falling through to the reject path
			// below exactly as a fresh rejection would (gt-6r4exl). An approve
			// waits for the bead's own turn; an unknown answer keeps the record
			// and a definite miss deletes it. Every case but the rejection ends
			// the pass with ErrNoStoredVerdict, so om never runs and no gate starts.
			switch {
			case reuseErr != nil:
				omDone()
				return reviewed{}, ErrNoStoredVerdict
			case !reused:
				if err := l.Ahead.Delete(w.BeadID); err != nil {
					omDone()
					return reviewed{}, &InfraError{Stage: "stored review", Err: err}
				}
				omDone()
				return reviewed{}, ErrNoStoredVerdict
			case !rec.rejecting():
				omDone()
				return reviewed{}, ErrNoStoredVerdict
			}
		}
		reused = reused && reuseErr == nil
		if reused {
			verdict = rec.Verdict
			verdict.Version = key.OMVersion
			reusedRec = &rec
			omDone()
			l.recordAcceptedRisks(w, used)
			reusedSuffix = omReusedSuffix(time.Duration(rec.ReviewSecs*float64(time.Second)), l.now().Sub(rec.ReviewedAt))
		} else {
			// The usage window opens with the review it measures (gt-6qc3nw.13); it
			// is read from the landing's clock, so a test can fix it.
			usageStart := l.now()
			verdict, reviewErr = l.review(omCtx, in)
			// One flaky om run must not cost an escalation and a hand review
			// (gt-q241r): an execution error on an ordinary tree is retried once,
			// inside this same om stage. A timeout, an infra error and a verdict
			// that arrived are final, and so is an execution error on a tree past
			// the size bound, which is deterministic and would fail the same way
			// again.
			omRetried = errors.Is(reviewErr, ErrOMExecution) && omCtx.Err() == nil && !l.omDiffPastBound(g, base, merged)
			if omRetried {
				l.logf("%s: om review returned no verdict (%s); retrying once", w.BeadID, reviewErrorReason(reviewErr))
				verdict, reviewErr = l.review(omCtx, in)
			}
			if reviewErr == nil {
				verdict = l.rescoreBorderline(omCtx, verdict, in)
			}
			omDone()
			omTime = time.Since(reviewStart)
			usageEnd := l.now()
			l.recordAcceptedRisks(w, used)
			if l.UsageSink != nil {
				l.UsageSink(ctx, ReviewUsage{BeadID: w.BeadID, Rig: w.Rig, Dir: dir, Start: usageStart, End: usageEnd})
			}
		}
	}
	if reviewErr == nil && verdict.Verdict != VerdictApprove && verdict.Verdict != VerdictRequestChanges && verdict.Verdict != VerdictSkipped && !strings.HasPrefix(verdict.Verdict, VerdictOverseerPrefix) {
		reviewErr = fmt.Errorf("reviewer returned no verdict (%q)", verdict.Verdict)
	}
	if reviewErr != nil {
		if ctx.Err() != nil || (!l.ReviewErrorLands && !l.ReviewErrorRejects) {
			l.logf("%s: %s", w.BeadID, stageTimesCISkipped(omNoVerdictStage, omTime, omRetried)+reusedSuffix)
			return reviewed{}, &InfraError{Stage: "review", Err: reviewErr}
		}
		if !l.ReviewErrorLands && !l.landsUnreviewed(issue) {
			l.logf("%s: %s", w.BeadID, stageTimesCISkipped(omRejectedStage, omTime, omRetried)+reusedSuffix)
			rej := &Rejection{Kind: RejectReview, Rework: false,
				Reason: l.reviewErrorRejectionReason(wt, base, merged, reviewErr)}
			return reviewed{}, l.reject(ctx, issue, w, rej, nil)
		}
		verdict = Verdict{Verdict: VerdictErrorPrefix + reviewErrorReason(reviewErr), Version: verdict.Version}
		l.logf("%s: WARNING om review did not run (%v); the candidate goes on to its gate with om_verdict %q", w.BeadID, reviewErr, verdict.Verdict)
	}
	// The review is over and this is its verdict, whatever the branches below
	// do with the landing: the work bead gets the one line that ties it to the
	// review, written before either the rejection or the merge (gt-uebmq). The
	// findings decide the approval's fate before the line is written, so an
	// approval they bounce is logged as the rejection it is (gt-vu18r).
	findingsRej := findingsRejection(verdict)
	l.logReview(issue, w, comments, g, base, merged, verdict, findingsRej != nil)
	l.logRounds(issue, w, verdict)
	// A reused verdict is commented in the form that names its stored review; a
	// record too incomplete for that form falls back to commentVerdict's rather
	// than going uncommented (gt-bydaep).
	if reusedRec == nil || !l.commentReused(w, verdict, *reusedRec) {
		l.commentVerdict(w, verdict, reviewStart, time.Now())
	}
	if verdict.Verdict == VerdictRequestChanges {
		l.logf("%s: %s", w.BeadID, stageTimesCISkipped(omRejectedStage, omTime, omRetried)+reusedSuffix)
		rej := &Rejection{Kind: RejectReview, Rework: true, Reason: fmt.Sprintf("om requested changes (score %.2f, %d finding(s))", verdict.Score, len(verdict.Findings))}
		return reviewed{}, l.reject(ctx, issue, w, rej, &verdict)
	}
	// An approving verdict is a score and a set of findings, and the findings
	// refuse the landing too: a major inside the approving band would otherwise
	// land on its score alone (gt-oi4je). The candidate is never pushed for
	// either refusal, so no gate run pays for it.
	if rej := findingsRej; rej != nil {
		l.logf("%s: %s", w.BeadID, stageTimesCISkipped(omRejectedStage, omTime, omRetried)+reusedSuffix)
		return reviewed{}, l.reject(ctx, issue, w, rej, &verdict)
	}
	if err := ctx.Err(); err != nil {
		return reviewed{}, &InfraError{Stage: "push", Err: err}
	}
	return reviewed{verdict: verdict, omTime: omTime, omRetried: omRetried, waived: waived, reusedSuffix: reusedSuffix}, nil
}
