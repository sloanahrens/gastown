package land

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
)

// mergeCar cuts the landing worktree at the target tip and makes the merge
// commit there, ending the pass when the merge conflicts, writes nothing, or
// leaves a state the landing cannot record. A nil car means the pass is over
// and the caller returns res and err; a non-nil cleanup means the worktree was
// added, and the caller defers it so every path removes the worktree.
//
// From the add on, a panic in the merge work removes the worktree here, since
// no cleanup reaches the caller; once the work has returned, cleanup belongs to
// the caller and is not run here.
func (l *Lander) mergeCar(ctx context.Context, g Repo, issue *beads.Issue, w Work, base string, storedConflict bool) (c *car, cleanup func(), res Result, err error) {
	// The merge commit's identity is read before the worktree is cut, so an
	// unreadable head author fails the landing with no commit made (gt-vuv2n).
	env, err := l.landingCommitEnv(g, w)
	if err != nil {
		return nil, nil, Result{}, &InfraError{Stage: "head author", Err: fmt.Errorf("head %s: %w", shortSHA(w.Head), err)}
	}
	// The worktree calls share gitMu with the review pool's git calls, so a
	// review's ref or worktree lock cannot collide with the add or with the
	// cleanup that removes the worktree (gt-havcnw). The lock is held only for
	// the add here and for the removal at the end, never over the merge, the om
	// run or the gate wait.
	var dir, id string
	if err := l.withGitMu(func() error {
		var addErr error
		dir, id, cleanup, addErr = l.addWorktree(g, base)
		return addErr
	}); err != nil {
		return nil, nil, Result{}, &InfraError{Stage: "worktree", Err: err}
	}
	returned := false
	defer func() {
		if !returned {
			cleanup()
		}
	}()
	ctx = WithLandingID(ctx, id)
	wt := l.openLanding(dir, env)

	c, res, err = l.mergeInWorktree(ctx, issue, g, w, base, storedConflict, dir, id, wt)
	returned = true
	return c, cleanup, res, err
}

// mergeInWorktree makes the merge in the landing worktree and reads its
// patch-id, ending the pass as mergeCar describes. It returns the car a clean
// merge carries on, or a nil car with the pass's result and error.
func (l *Lander) mergeInWorktree(ctx context.Context, issue *beads.Issue, g Repo, w Work, base string, storedConflict bool, dir, id string, wt Repo) (*car, Result, error) {
	remote := l.remote()
	out, err := mergeWork(wt, w, base)
	if err != nil {
		// The last shape of a lost record: a squash whose head the target
		// already carries stages nothing, so mergeWork's commit fails with an
		// empty index and every pass reports the merge as infra over a landing
		// that already took (gt-064oh). mergeWork has aborted the merge, so
		// the worktree is clean whether or not the recovery finds a landing,
		// and the merge's own error is the one to report when it does not.
		// A lookup that fails says nothing about whether the landing exists,
		// so the pass ends as the infra error the worker retries; rejecting
		// here would send the author to rework work that may have landed
		// (gt-xqpmc).
		if res, handled, cerr := l.finishedUnrecordedLanding(g, issue, w); handled {
			return nil, res, cerr
		}
		// No land merge accounts for the head's content on the target, and
		// merging it changes nothing: the work is already there by a route
		// that left no record, so there is nothing for the author to land
		// and nothing for an operator to repair (gt-sq2oe).
		if out.EmptySquash {
			return nil, Result{}, l.reject(ctx, issue, w, &Rejection{Kind: RejectEmpty, Rework: true, Reason: EmptyMergeReason(wt, EmptyMerge{
				Target: w.Target, Base: base, Head: w.Head, Stage: "after merge",
				Comparison: fmt.Sprintf("merging %s changes nothing on %s/%s (its changes are already there, and no land merge of this lander's accounts for them)", shortSHA(w.Head), remote, w.Target),
			})}, nil)
		}
		return nil, Result{}, err
	}
	merged, rej := out.Commit, out.Rejection
	if rej != nil {
		// A conflict with the head's own landed squash is the landing meeting
		// itself, not the author's to resolve; with no land merge naming this
		// head on the target the recovery declines and the rejection stands
		// (gt-01yt6).
		if rej.Kind == RejectConflict {
			if res, handled, err := l.finishedUnrecordedLanding(g, issue, w); handled {
				return nil, res, err
			}
			// A stored conflict for this head that the merge still finds is
			// spent: the rejection is its answer, and the record must not
			// outlive it (gt-1yvte7). A delete that fails is logged and the
			// rejection stands: the author's note is what the queue needs, and
			// a later pass deletes the record as a mismatch once a new head is
			// pushed.
			if storedConflict {
				if err := l.Ahead.Delete(w.BeadID); err != nil {
					l.logf("%s: the rejected conflict record could not be deleted (%v); the rejection stands", w.BeadID, err)
				}
			}
		}
		return nil, Result{}, l.reject(ctx, issue, w, rej, nil)
	}
	// The stored conflict was for this head and the merge no longer conflicts,
	// so the record is consumed and the pass ends before any review or gate
	// (gt-1yvte7).
	if storedConflict {
		if err := l.Ahead.Delete(w.BeadID); err != nil {
			return nil, Result{}, &InfraError{Stage: "stored review", Err: err}
		}
		l.logf("%s: the stored conflict is gone; the clean merge consumes its record", w.BeadID)
		return nil, Result{}, ErrNoStoredVerdict
	}
	if same, err := wt.TreesIdentical(base, "HEAD"); err != nil {
		return nil, Result{}, &InfraError{Stage: "compare trees", Err: err}
	} else if same {
		return nil, Result{}, l.reject(ctx, issue, w, &Rejection{Kind: RejectEmpty, Rework: true, Reason: EmptyMergeReason(wt, EmptyMerge{
			Target: w.Target, Base: base, Head: w.Head, Stage: "after merge",
			Comparison: fmt.Sprintf("the merge result and %s/%s have identical trees", remote, w.Target),
		})}, nil)
	}
	patchID, err := wt.PatchID(base, merged)
	if err != nil {
		return nil, Result{}, &InfraError{Stage: "patch-id", Err: err}
	}

	// The car carries the worktree and the landing's identity to the later
	// stages.
	return &car{w: w, issue: issue, g: g, wt: wt, base: base, merged: merged, patchID: patchID, dir: dir, landingID: id}, Result{}, nil
}
