package land

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// prechecked is what the checks before the merge leave for the merge and the
// later stages: the bead and the rig repo, the target tip the merge is built
// on, the risk paths the landing carries and the rule that skipped their
// review, and whether a stored conflict for this head is waiting on the merge.
type prechecked struct {
	issue          *beads.Issue
	g              Repo
	base           string
	riskPaths      []string
	skippedBy      string
	storedConflict bool
}

// precheckCar runs the checks before the merge of w: the bead's readiness and
// policy, the repair of an incomplete record, the head's push, the ancestry and
// range checks, the risk paths and the tree comparison. A nil p means the pass
// is over and the caller returns res and err; a non-nil p carries the checks'
// results to mergeCar and the later stages.
func (l *Lander) precheckCar(ctx context.Context, w Work) (p *prechecked, res Result, err error) {
	if err := l.validate(w); err != nil {
		return nil, Result{}, err
	}
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return nil, Result{}, &InfraError{Stage: "read bead", Err: err}
	}
	remote := l.remote()
	g := l.open(l.Repo)
	// A landing whose bead record was left incomplete (RecordError) is
	// finished from the landings file, never landed twice. This runs before
	// the readiness checks, because the partial record may already have
	// taken the ready label off.
	if repaired, err := l.repairRecord(g, w); err != nil {
		if repaired != nil {
			return nil, *repaired, err
		}
		return nil, Result{}, err
	} else if repaired != nil {
		return nil, *repaired, nil
	}
	// IsActionable, not IsTerminal: a defer applied while the gate ran parks
	// the bead under a worker that has already read it (gt-y7n1u).
	if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() {
		return nil, Result{}, fmt.Errorf("%w: %s is %s", ErrNotReady, w.BeadID, issue.Status)
	}
	if !beads.HasLabel(issue, LabelReadyToLand) {
		return nil, Result{}, fmt.Errorf("%w: %s has no %s label", ErrNotReady, w.BeadID, LabelReadyToLand)
	}
	if rej := policyRejection(issue); rej != nil {
		return nil, Result{}, l.reject(ctx, issue, w, rej, nil)
	}

	// A stored-only pass consumes a stored answer and nothing else. A stored
	// conflict that cannot answer for this landing — another head, or past
	// ReuseExpiry — is deleted here, before the target fetch and the merge, and
	// the pass ends; only a conflict for this head goes on to the merge, which
	// decides whether it is still there (gt-1yvte7).
	storedConflict, err := l.storedOnlyConflictBeforeMerge(w)
	if err != nil {
		return nil, Result{}, err
	}

	if err := l.withGitMu(func() error { return l.fetchTarget(g, w.Target) }); err != nil {
		return nil, Result{}, &InfraError{Stage: "fetch target", Err: err}
	}
	base, err := g.Rev(remote + "/" + w.Target)
	if err != nil {
		return nil, Result{}, &InfraError{Stage: "resolve target", Err: err}
	}
	// The town deletes a branch once it merges its landing, so a retry over a
	// merge that lost its record finds no branch left to check (gt-4av3vd).
	if rej, err := l.checkHeadPushed(g, w); err != nil || rej != nil {
		if rej != nil {
			if rej.BranchGone {
				res, err := l.finishedGoneBranchLanding(g, issue, w, base)
				if res != nil {
					return nil, *res, err
				}
				if err != nil {
					return nil, Result{}, err
				}
			}
			return nil, Result{}, l.reject(ctx, issue, w, rej, nil)
		}
		return nil, Result{}, err
	}
	already, err := g.IsAncestor(w.Head, base)
	if err != nil {
		return nil, Result{}, &InfraError{Stage: "ancestry", Err: err}
	}
	// A landing this lander merged but never recorded — the daemon died
	// between the merge and the record, or the MergePull response was lost —
	// is finished, not landed again. The land merge's subject names this
	// branch, head and target, so a search of the target finds it, and the
	// landing is not the author's work to redo.
	//
	// The search runs only where the target already carries this head's work:
	// the head is on the target (a --no-ff merge contains it, so the ancestry
	// check above saw it), the two trees are identical, or a re-merge writes
	// nothing (a squash, which mergeWork makes of a range that still holds
	// auto-save commits, is a single-parent commit that never contains the
	// head, so neither cheap test can see it). Those are the three dead ends
	// the pass would otherwise end on, and each one asks the recovery before
	// it gives up. A landing that reaches none of them is a landing to make,
	// and never walks the target's history for a land merge naming a head
	// nothing merged (gt-064oh). A fourth shape — a retry whose merge
	// conflicts with a later landing that edited the head's own lines — still
	// ends as a conflict rejection with rework (gt-01yt6).
	if already {
		if res, handled, err := l.finishedUnrecordedLanding(g, issue, w); handled {
			return nil, res, err
		}
		// The head is on the target with no land merge of this lander's to
		// account for it: it reached the target by a route that left no
		// landing record (an operator push), which is not the author's to
		// rework (gt-mdet3).
		return nil, Result{}, l.reject(ctx, issue, w, &Rejection{Kind: RejectEmpty, Rework: false,
			Reason: fmt.Sprintf("empty merge: head %s is already reachable from %s/%s (%s) with no landing record; nothing to land", shortSHA(w.Head), remote, w.Target, shortSHA(base))}, nil)
	}
	for _, check := range l.RangeChecks {
		rej, err := check(g, base, w.Head)
		if err != nil {
			return nil, Result{}, &InfraError{Stage: "range check", Err: err}
		}
		if rej != nil {
			return nil, Result{}, l.reject(ctx, issue, w, rej, nil)
		}
	}
	// Risk paths are the label this landing carries, never a hold: a read
	// that fails leaves the landing unlabelled rather than stopping it, so a
	// broken globs file can never wedge the queue (gt-vsct7.4).
	riskPaths, err := RiskPaths(g, base, w.Head)
	if err != nil {
		l.logf("%s: reading risk paths: %v; landing unlabelled", w.BeadID, err)
		riskPaths = nil
	}
	// A bead the review machinery filed records no risk path, so its landing
	// asks for no risk-path review of the review's own output (gt-g349w).
	riskPaths, skippedBy := RiskPathsRecorded(issue, riskPaths)
	if skippedBy != "" {
		l.logf("%s: risk-path review skipped: the review machinery filed this bead (%s); recording no risk path", w.BeadID, skippedBy)
	}
	if same, err := g.TreesIdentical(base, w.Head); err != nil {
		return nil, Result{}, &InfraError{Stage: "compare trees", Err: err}
	} else if same {
		// Identical trees before the merge are the other shape of a lost
		// record rather than an empty range: the squash of a head that forked
		// at the target writes exactly the head's tree, so the target and the
		// head compare equal and the author would be sent to rework work that
		// already landed (gt-064oh). The range checks above already ran; they
		// read the base..head the head's own first pass read and passed, so
		// they do not stand between a lost record and its recovery here.
		if res, handled, err := l.finishedUnrecordedLanding(g, issue, w); handled {
			return nil, res, err
		}
		return nil, Result{}, l.reject(ctx, issue, w, &Rejection{Kind: RejectEmpty, Rework: true, Reason: EmptyMergeReason(g, EmptyMerge{
			Target: w.Target, Base: base, Head: w.Head, Stage: "before merge",
			Comparison: fmt.Sprintf("%s/%s and %s have identical trees", remote, w.Target, shortSHA(w.Head)),
		})}, nil)
	}
	return &prechecked{issue: issue, g: g, base: base, riskPaths: riskPaths,
		skippedBy: skippedBy, storedConflict: storedConflict}, Result{}, nil
}
