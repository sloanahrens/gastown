package land

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// car is what one landing carries between its stages: the work and its bead,
// the rig repo and the landing worktree the merge is made in, the target tip
// the merge was built on, the merge commit, and that commit's patch-id.
type car struct {
	w                     Work
	issue                 *beads.Issue
	g, wt                 Repo
	base, merged, patchID string
}

// finishCar ends a landing whose candidate gate passed: the merge through
// Forgejo, its read-back, the record of the landing, and the follow-up feed.
func (l *Lander) finishCar(ctx context.Context, c *car, res Result, verdict Verdict, ciContext string, cand *CandidateResult) (Result, error) {
	w, issue, g, wt, base, merged, patchID := c.w, c.issue, c.g, c.wt, c.base, c.merged, c.patchID
	remote := l.remote()
	// The rig lands through Forgejo: the om status is posted and the PR merged
	// with head_commit_id, so a candidate that moved after the verdict cannot
	// land on it (design, "The PR, the merge and the forgery check"). The merge
	// is a fast-forward, so the target's tip is the candidate commit the
	// read-back checks.
	//
	// The merge verifies the candidate's status creators first, so it needs the
	// context CI reported: a candidate gate that reported none is a
	// misconfiguration rather than a landing.
	if ciContext == "" {
		return Result{}, &InfraError{Stage: "merge pull request",
			Err: errors.New("the candidate gate reported no status context, so the merge's creator check has nothing to verify")}
	}
	// The candidate build and the review take minutes: a bead closed or stripped
	// of its ready label in that time is a withdrawal, and the merge must not
	// land it (gt-b3zpjb). A read that fails refuses too: an unknown state does
	// not merge, and the infrastructure error retries the landing.
	cur, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return Result{}, &InfraError{Stage: "re-read bead before merge", Err: err}
	}
	if !beads.IssueStatus(strings.TrimSpace(cur.Status)).IsActionable() || !beads.HasLabel(cur, LabelReadyToLand) {
		l.discardCandidate(ctx, w, cand)
		return Result{}, fmt.Errorf("%w: %s changed while its candidate was built (bead closed or %s removed: status %s)", ErrNotReady, w.BeadID, LabelReadyToLand, cur.Status)
	}
	if err := l.Merger.Merge(ctx, MergeRequest{Work: w, Head: merged, Verdict: verdict, GateContext: ciContext,
		TargetHasCommit: l.targetHasCommit(g, w.Target)}); err != nil {
		var refused *MergeRefusedError
		var forged *ForgedStatusError
		switch {
		case errors.As(err, &forged):
			// Tampering with the gate is nobody's to rework and no retry
			// lifts it: the bead goes to a human, and the worker escalates
			// it (classification reads Rework=false as outRejectedHuman).
			rej := &Rejection{Kind: RejectForgedStatus, Rework: false, Reason: forged.Reason}
			return Result{}, l.reject(ctx, issue, w, rej, nil)
		case errors.As(err, &refused):
			rej := &Rejection{Kind: RejectMergeRefused, Rework: false,
				Reason: fmt.Sprintf("Forgejo refused to merge %s into %s (405, not ready to be merged): %s. A required status is missing or red, so a retry cannot converge: check that branch protection's required contexts match the gate workflow's, and that the %s status was accepted. A human decides.",
					w.Candidate(), w.Target, elideMiddle(NoteField(refused.Err.Error()), reviewErrorReasonMax), OMStatusContext)}
			return Result{}, l.reject(ctx, issue, w, rej, nil)
		}
		return Result{}, err
	}
	if err := wt.VerifyPushedCommitReachableFromPushTarget(remote, w.Target, merged); err != nil {
		return Result{}, &InfraError{Stage: "read-back", Err: fmt.Errorf("%w: %v", ErrReadBack, err)}
	}
	l.logf("%s: landed %s on %s/%s (patch-id %s)", w.BeadID, shortSHA(merged), remote, w.Target, shortSHA(patchID))

	if l.DeferBookkeeping {
		// The ref and the landings line are the record a crash must not lose;
		// the bead half reads the record back, so it is the later pass's work.
		rec, err := l.recordDurable(g, w, res)
		if err != nil {
			// Deferred stays nil on error: the repair path finishes only a record
			// the landings file holds.
			return res, &RecordError{Result: res, Err: err}
		}
		res.Deferred = &rec
		return res, nil
	}
	if err := l.record(g, w, res); err != nil {
		return res, &RecordError{Result: res, Err: err}
	}
	// The landing is recorded, so the feed runs after it and cannot change it
	// (gt-s01pn).
	l.fileFollowups(g, base, w, res)
	return res, nil
}
