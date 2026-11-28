package land

import (
	"context"
	"fmt"
	"time"
)

// reviewed is what the om review leaves for the gate: its verdict, how long om
// took and whether it was retried, whether the overseer waived the head, and
// the reuse suffix of the stage line.
type reviewed struct {
	verdict      Verdict
	omTime       time.Duration
	omRetried    bool
	waived       bool
	reusedSuffix string
}

// gated is what the candidate gate leaves for the finish: its result, the
// status context CI reported, and the candidate gate's run, whose pushed
// branch is discarded if the landing ends.
type gated struct {
	res       GateResult
	ciContext string
	cand      *CandidateResult
}

// gateCar runs the candidate gate for a car whose om review passed, and
// refuses the landing when the gate is red or reaches no verdict.
func (l *Lander) gateCar(ctx context.Context, c *car, r reviewed) (gated, error) {
	w, wt, dir, merged, base, issue := c.w, c.wt, c.dir, c.merged, c.base, c.issue
	// om passed, so the candidate is pushed and its CI waited on. A red gate
	// after an om pass is a gate refusal: the om verdict is already recorded
	// on the bead, and it counts no review round.
	l.stage(w, StageCI)
	gateCtx, gateDone := l.Slow.watch(ctx, l, w, dir, StageCI)
	var (
		gateRes   GateResult
		gateErr   error
		ciContext string
		// candRes is the candidate gate's run: the branch it pushed is
		// discarded if the gate ends the landing.
		candRes *CandidateResult
	)
	if c.stored != nil {
		// A stored run is rendered, not gated: no candidate gate runs, so there
		// is no commit to announce as about to be gated (gt-kbzz2i).
		run := *c.stored
		candRes = &run
		gateRes, gateErr = l.renderCandidate(w, run, 0)
	} else {
		// The candidate is announced just before its gate runs, so the hook sees
		// the commit being gated ahead of the verdict (gt-49dh3n).
		if l.CandidateBuilt != nil {
			l.CandidateBuilt(w, merged)
		}
		run, ciVerdict, err := l.candidateGate(gateCtx, wt, dir, w, merged)
		candRes, gateRes, gateErr = &run, ciVerdict, err
	}
	ciContext = ciGateContext(gateRes)
	gateDone()
	if gateErr != nil {
		// om ran and its time is logged even though the gate never reached a
		// verdict, as the red path logs it.
		l.logf("%s: %s", w.BeadID, stageTimes(gateRes, r.omTime, r.omRetried)+r.reusedSuffix)
		l.discardCandidate(ctx, w, candRes)
		return gated{}, gateErr
	}
	if gateRes.Err != nil {
		return gated{}, &InfraError{Stage: "gate", Err: gateRes.Err}
	}
	stageLine := stageTimes(gateRes, r.omTime, r.omRetried)
	if r.waived {
		stageLine += ", om skipped (overseer-reviewed)"
	}
	stageLine += r.reusedSuffix
	l.logf("%s: %s", w.BeadID, stageLine)
	if !gateRes.Passed {
		reason, tail := "gate failed on the merged tree: "+gateRes.Summary(), gateRes.FailureTail()
		if len(gateRes.Steps) > 0 {
			reason = fmt.Sprintf("the candidate gate %s failed on the merged tree", gateRes.Steps[0].Command)
		}
		l.discardCandidate(ctx, w, candRes)
		// The refusal carries the verdict it followed, though that verdict
		// approved: no block records an approval, so the next review reads the
		// findings here or runs cold on the unchanged diff (gt-g8otg). The
		// approval is marked too, so a clean approve still answers its round
		// and the walk does not reach an older review (gt-xal5q). A waived
		// head and a review that reached no verdict leave both empty.
		// base is the target tip the merged tree was built on: the gate judged
		// that tree, so a later target tip is what lifts a resubmission.
		rej := &Rejection{Kind: RejectGate, Rework: true, Reason: reason, GateTail: tail, GateLog: candRes.GateLog,
			Findings: r.verdict.Findings, Approved: r.verdict.Verdict == VerdictApprove, TargetTip: base}
		return gated{}, l.reject(ctx, issue, w, rej, nil)
	}
	return gated{res: gateRes, ciContext: ciContext, cand: candRes}, nil
}
