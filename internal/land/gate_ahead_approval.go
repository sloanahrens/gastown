package land

import (
	"context"
	"errors"
	"fmt"
)

// gateAheadApproval answers whether the candidate built by merging w onto base
// in the worktree at dir may be gated ahead of its turn (gt-0hz0sy): nil only
// when a stored review reuseAhead accepts for the landing's own input approves
// the merge. It writes nothing, and a lander with no review-ahead store refuses
// rather than read the missing store as an approval.
// The input is reviewInputFor's strict read, as aheadInput reads it for the
// pool. A merge that edits the root .om.json waits for the overseer, and is
// refused too (gt-93hf4).
func (l *Lander) gateAheadApproval(g Repo, w Work, base, dir, merged string) error {
	if l.Ahead == nil {
		return errors.New("no review-ahead store is configured, so no stored review can approve the candidate")
	}
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return fmt.Errorf("reading bead %s: %w", w.BeadID, err)
	}
	if issue == nil {
		return fmt.Errorf("reading bead %s: no bead returned", w.BeadID)
	}
	if rej := l.omConfigRejection(g, base, merged, w.Head, w.BeadID); rej != nil {
		return errors.New(rej.Reason)
	}
	in, _, _, err := l.reviewInputFor(issue, w, base, merged, dir, g, true)
	if err != nil {
		return fmt.Errorf("the review input could not be assembled: %w", err)
	}
	rec, _, hit, err := l.reuseAhead(context.Background(), g, l.open(dir), w, in, base, merged)
	if err != nil {
		return fmt.Errorf("the stored review could not be read: %w", err)
	}
	if !hit {
		if rec.Outcome == "" {
			return fmt.Errorf("no stored review is kept for %s", w.BeadID)
		}
		return fmt.Errorf("the stored review of %s is not reusable (outcome %q, verdict %q)", shortSHA(merged), rec.Outcome, rec.Verdict.Verdict)
	}
	if rec.Verdict.Verdict != VerdictApprove {
		return fmt.Errorf("the stored review of %s does not approve the merge (verdict %q, score %.2f)", shortSHA(merged), rec.Verdict.Verdict, rec.Verdict.Score)
	}
	// The landing refuses an approve whose findings block it (gt-oi4je), so a
	// candidate approved only by such a verdict must not be gated ahead.
	if rej := findingsRejection(rec.Verdict); rej != nil {
		return fmt.Errorf("the stored review of %s approves with findings that block the landing: %s", shortSHA(merged), rej.Reason)
	}
	return nil
}
