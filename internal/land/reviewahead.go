package land

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// AheadOutcome is what one review made ahead of a landing came to.
type AheadOutcome string

const (
	// AheadStored means the review's verdict was kept in the review-ahead
	// store, for a later landing to reuse.
	AheadStored AheadOutcome = "stored"
	// AheadConflict means the submission does not merge onto the target, so no
	// verdict was made: a conflict record was stored instead.
	AheadConflict AheadOutcome = "conflict"
	// AheadSkipped is a submission no review was made for. The landing runs om
	// inline, as it does without a review-ahead store.
	AheadSkipped AheadOutcome = "skipped"
)

// aheadSourcePool is the Source an AheadRecord carries when the review pool
// made it ahead of its landing, rather than a landing running om inline.
const aheadSourcePool = "pool"

// ReviewAhead makes the review of w that a later landing reuses, ahead of
// that landing. It never writes to Beads: only the serial landing changes a
// bead. A nil Ahead store means the feature is off and nothing is read.
//
// The outcome is empty when err is set, so a caller that switches on it
// cannot mistake a failed read for a skipped review.
func (l *Lander) ReviewAhead(ctx context.Context, w Work) (AheadOutcome, error) {
	outcome, _, err := l.reviewAhead(ctx, w)
	return outcome, err
}

// aheadBuilt is the review input and key a clean merge's review is made for:
// In is the input Land's om stage hands om for the same bead and merge, and Key
// is the key reviewKey computes from it. reviewAhead returns it so a test can
// hold both against what the landing builds.
//
// In.Dir is empty: it names the review checkout the input was built in, and
// cleanup removes that worktree before reviewAhead returns, so the returned
// input names no checkout that has gone.
type aheadBuilt struct {
	In  ReviewInput
	Key ReviewKey
}

// reviewAhead is ReviewAhead with the input and key it built. Only a clean merge
// whose review is made returns a built value; every other outcome and every error
// returns the zero aheadBuilt.
func (l *Lander) reviewAhead(ctx context.Context, w Work) (AheadOutcome, aheadBuilt, error) {
	if l.Ahead == nil {
		return AheadSkipped, aheadBuilt{}, nil
	}
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return "", aheadBuilt{}, &InfraError{Stage: "read bead", Err: err}
	}
	if issue == nil {
		return "", aheadBuilt{}, &InfraError{Stage: "read bead", Err: errors.New("no bead returned for " + w.BeadID)}
	}
	// The same readiness tests Land makes, in the same order, so a bead the
	// pool reviews is one the landing would not skip.
	if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() {
		return AheadSkipped, aheadBuilt{}, nil
	}
	if !beads.HasLabel(issue, LabelReadyToLand) {
		return AheadSkipped, aheadBuilt{}, nil
	}
	if policyRejection(issue) != nil {
		return AheadSkipped, aheadBuilt{}, nil
	}
	// The overseer reviewed this head in place of om, so it needs no review.
	if OverseerReviewed(issue, w.Head) {
		return AheadSkipped, aheadBuilt{}, nil
	}
	// started is when this review began: the land log dates it, and a stored
	// record keeps it as started_at.
	started := l.aheadNow()
	l.logf("%s: ahead review of %s started at %s", w.BeadID, shortSHA(w.Head), started.UTC().Format(time.RFC3339))
	g := l.open(l.Repo)
	if err := l.aheadFetches(g, w); err != nil {
		return "", aheadBuilt{}, err
	}
	// The base is the target tip the fetch just brought in, resolved the way
	// Land resolves it before it cuts its own worktree.
	base, err := g.Rev(l.remote() + "/" + w.Target)
	if err != nil {
		return "", aheadBuilt{}, &InfraError{Stage: "resolve target", Err: err}
	}
	// addReviewWorktree takes gitMu for the git calls it makes, and the lock is
	// not reentrant: aheadFetches has released it, so this call takes it
	// itself. Holding it here would deadlock the add.
	wt, dir, cleanup, err := l.addReviewWorktree(g, w, base)
	if err != nil {
		return "", aheadBuilt{}, &InfraError{Stage: "review worktree", Err: err}
	}
	// Every return below this line, and a panic's unwind, removes the worktree
	// and the review directory that holds it.
	defer cleanup()
	l.logf("%s: ahead review worktree at %s", w.BeadID, dir)
	// mergeWork reports a conflict the way Land reads it: a Rejection with a
	// nil error and the merge aborted.
	out, err := mergeWork(wt, w, base)
	if err != nil {
		return "", aheadBuilt{}, err
	}
	if out.Rejection != nil {
		// The submission does not merge onto the target, so no verdict can be
		// made. The conflict is kept so a reader can tell a review that found a
		// conflict from one that never ran; the landing still decides the
		// rejection itself.
		l.logf("%s: ahead merge of %s conflicts in %d file(s)", w.BeadID, shortSHA(w.Head), len(out.Rejection.Conflicting))
		rec := AheadRecord{
			Bead:       w.BeadID,
			Head:       w.Head,
			Base:       base,
			StartedAt:  started,
			ReviewedAt: l.aheadNow(),
			Outcome:    aheadOutcomeConflict,
			Source:     aheadSourcePool,
		}
		if err := l.Ahead.Put(rec); err != nil {
			return "", aheadBuilt{}, &InfraError{Stage: "store record", Err: err}
		}
		return AheadConflict, aheadBuilt{}, nil
	}
	merged := out.Commit
	// A merge that leaves the target's tree unchanged is what Land refuses as
	// empty, so om has nothing to judge and there is nothing to store.
	same, err := wt.TreesIdentical(base, merged)
	if err != nil {
		return "", aheadBuilt{}, &InfraError{Stage: "compare trees", Err: err}
	}
	if same {
		l.logf("%s: ahead merge of %s leaves %s/%s unchanged", w.BeadID, shortSHA(w.Head), l.remote(), w.Target)
		return AheadSkipped, aheadBuilt{}, nil
	}
	// A landing that edits the root .om.json changes the bar om judges it
	// against, so it waits for the overseer rather than om (gt-93hf4): the
	// pool makes no verdict for it. The paths are read here rather than through
	// land.go's omConfigRejection because its fail-closed refusal suits the
	// landing and not the pool: a diff that cannot be read has to surface as an
	// error, or a caller switching on the outcome reads the failed read as a
	// submission with nothing to review.
	paths, err := g.DiffNameOnly(base, merged)
	if err != nil {
		return "", aheadBuilt{}, &InfraError{Stage: "read changed paths", Err: err}
	}
	if slices.Contains(paths, omConfigFile) {
		l.logf("%s: ahead merge of %s edits %s, which waits for the overseer", w.BeadID, shortSHA(w.Head), omConfigFile)
		return AheadSkipped, aheadBuilt{}, nil
	}
	// The input and key are built before om runs, so a key that cannot be
	// computed costs no review.
	// reviewKey runs off gitMu. Its git reads are of the base and merged commits,
	// which do not change, and the om config it reads is a file, not a git
	// write. Its om version probe is a subprocess that must not hold the lock.
	in, err := l.aheadInput(issue, w, base, merged, dir, g)
	if err != nil {
		return "", aheadBuilt{}, err
	}
	key, _, err := l.reviewKey(ctx, g, wt, w, in, base, merged)
	if err != nil {
		return "", aheadBuilt{}, &InfraError{Stage: "review key", Err: err}
	}
	// The checkout goes with the worktree the deferred cleanup removes, so the
	// returned input names none.
	built := aheadBuilt{In: in, Key: key}
	built.In.Dir = ""
	v, ran, err := l.aheadReview(ctx, in)
	if err != nil {
		return "", aheadBuilt{}, err
	}
	if !ran {
		// A reviewer the pool cannot isolate cannot run beside a landing, so no
		// review is made: the landing runs om inline as it does without a store.
		l.logf("%s: ahead review of %s skipped: the reviewer cannot be isolated", w.BeadID, shortSHA(w.Head))
		return AheadSkipped, aheadBuilt{}, nil
	}
	// Only a verdict om itself reached may be reused: an approval or a request
	// for changes. Any other word (a skipped review, an om that could not run,
	// an empty or unknown value) decided nothing the landing can reuse, so
	// nothing is stored and the landing runs om inline as it does without a
	// store. The store would refuse such a record too; skipping here is what
	// makes the outcome AheadSkipped rather than a store error.
	if v.Verdict != VerdictApprove && v.Verdict != VerdictRequestChanges {
		l.logf("%s: ahead review of %s skipped: verdict %q is not approve or request_changes", w.BeadID, shortSHA(w.Head), v.Verdict)
		return AheadSkipped, aheadBuilt{}, nil
	}
	// The verdict is kept for a later landing to reuse. It is not logged and no
	// round is written: the landing writes a review when it consumes a verdict.
	reviewed := l.aheadNow()
	rec := AheadRecord{
		Bead:         key.Bead,
		Head:         key.Head,
		PatchID:      key.PatchID,
		OMVersion:    key.OMVersion,
		OMConfigHash: key.OMConfigHash,
		InputDigest:  key.InputDigest,
		Base:         base,
		StartedAt:    started,
		ReviewedAt:   reviewed,
		ReviewSecs:   reviewed.Sub(started).Seconds(),
		Outcome:      aheadOutcomeVerdict,
		Verdict:      v,
		Source:       aheadSourcePool,
		// Verdict.Rescore is not serialized, so this field is the record's only
		// trace of a borderline rejection that was re-scored: true exactly when
		// rescoreBorderline stamped the standing verdict with a Rescore.
		Rescored: v.Rescore != nil,
	}
	if err := l.Ahead.Put(rec); err != nil {
		return "", aheadBuilt{}, &InfraError{Stage: "store record", Err: err}
	}
	return AheadStored, built, nil
}

// aheadReview runs om over in's checkout: the review, and the re-score of a
// borderline rejection, both through a copy of the reviewer isolated under the
// review directory that holds the checkout. ran is false when the reviewer
// cannot be isolated: such a reviewer cannot run beside a landing, so no review
// is made and the landing runs om inline. It is reached only by a Lander with an
// Ahead store, so a rig with the review pool off spends no om run here.
//
// review and rescoreBorderline read only a Lander's Reviewer and Out, so they
// run on a second Lander holding the isolated reviewer and l.Out. It is built
// field by field because copying l would copy its gitMu.
func (l *Lander) aheadReview(ctx context.Context, in ReviewInput) (Verdict, bool, error) {
	iso, ok := l.Reviewer.(Isolator)
	if !ok {
		return Verdict{}, false, nil
	}
	pool := &Lander{Reviewer: iso.Isolated(filepath.Dir(in.Dir)), Out: l.Out}
	v, err := pool.review(ctx, in)
	if err != nil {
		return Verdict{}, false, &InfraError{Stage: "review", Err: err}
	}
	return pool.rescoreBorderline(ctx, v, in), true, nil
}

// aheadInput assembles the review input for w through the helper Land's om stage
// calls (reviewInputFor), so the input and the key made from it are the
// landing's rather than a copy of them.
//
// Where Land logs an unreadable comment or settings file and reviews without
// it, this asks for the strict read: the pool makes no key for an input it
// cannot read in full, so the failure is returned. The rounds text is the
// exception, by design: roundsText turns a ledger read error into "", on both
// sides, which is the input om is given.
func (l *Lander) aheadInput(issue *beads.Issue, w Work, base, merged, dir string, g Repo) (ReviewInput, error) {
	in, _, _, err := l.reviewInputFor(issue, w, base, merged, dir, g, true)
	return in, err
}

// aheadNow is the clock a review is dated by: the store's clock when it sets
// one, the wall clock otherwise, as AheadStore documents.
func (l *Lander) aheadNow() time.Time {
	if l.Ahead != nil && l.Ahead.Now != nil {
		return l.Ahead.Now()
	}
	return time.Now()
}

// aheadFetches brings the target and the work branch into the lander's clone,
// the two fetches Land makes before it checks the head out. Each fetch takes
// the shared git lock for its own run: fetchTarget is wrapped in withGitMu
// here, and checkHeadPushed takes the lock for its branch fetch itself. The
// lock is not held across both calls, because withGitMu is not reentrant; the
// worktree add that follows takes it the same way.
//
// A submission the landing would refuse as not pushed has no head to review, so
// its rejection is returned as an error: the pool writes no bead and rejects
// nothing, and the serial landing's own check runs the rejection for real.
func (l *Lander) aheadFetches(g Repo, w Work) error {
	if err := l.withGitMu(func() error { return l.fetchTarget(g, w.Target) }); err != nil {
		return &InfraError{Stage: "fetch target", Err: err}
	}
	rej, err := l.checkHeadPushed(g, w)
	if err != nil {
		return err
	}
	if rej != nil {
		return rej
	}
	return nil
}
