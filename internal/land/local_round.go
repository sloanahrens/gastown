package land

import (
	"context"
	"errors"
)

// RunLocalRound runs the landing's om review over the polecat's own diff and
// returns its findings, writing nothing (gt-w4rvi2). in is the ReviewInput
// BuildReviewInput assembled for the work bead, with Base at the merge base of
// main and Head at the polecat's tip, so om judges the diff the polecat wrote
// and nothing that landed since.
//
// It is the landing's review reached before landing, and it records nothing: no
// verdict, no review-log record and no rounds-ledger round. Those are the
// landing's records, and a preview that wrote one would record a verdict for a
// diff that never landed. None of the three is a parameter, so the round cannot
// reach one.
//
// The bound is the caller's: RunLocalRound passes ctx to the reviewer and adds
// no timer, sleep or retry. Every failure — a reviewer error, an om execution
// error or the caller's bound expiring on ctx — comes back unchanged with no
// findings, for the caller to report as "preview unavailable". A missing
// reviewer is refused the same way: an error, never a panic, and a failure is
// never returned as a clean review.
func RunLocalRound(ctx context.Context, r BeadReviewer, in ReviewInput) ([]Finding, error) {
	if r == nil {
		return nil, errors.New("local round: no reviewer")
	}
	v, err := r.ReviewBead(ctx, in)
	if err != nil {
		return nil, err
	}
	return v.Findings, nil
}
