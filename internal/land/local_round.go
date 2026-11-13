package land

import (
	"context"
	"errors"
	"reflect"
)

// RunLocalRound runs the landing's om review over the polecat's own diff and
// returns its findings, or an error with no findings, writing nothing
// (gt-w4rvi2). in is the ReviewInput BuildReviewInput assembled for the work
// bead, with Base at the merge base of main and Head at the polecat's tip, so
// om judges the diff the polecat wrote and nothing that landed since.
//
// It is the landing's review reached before landing, and it records nothing: no
// verdict, no review-log record and no rounds-ledger round. Those are the
// landing's records, and a preview that wrote one would record a verdict for a
// diff that never landed. The signature admits none of the three, so the round
// cannot reach one.
//
// The bound is the caller's: RunLocalRound passes ctx to the reviewer and adds
// no timer, sleep or retry. An expired bound yields no findings, whether it
// expired before the call or during it, because the round checks ctx on both
// sides of the review and a reviewer is free to ignore it. Every failure — a
// reviewer error, an om execution error or the expired bound — comes back
// unchanged with no findings, for the caller to report as "preview
// unavailable". A missing reviewer, nil or a nil value held in the interface, is
// refused the same way: an error, never a panic, and a failure is never returned
// as a clean review.
func RunLocalRound(ctx context.Context, r BeadReviewer, in ReviewInput) ([]Finding, error) {
	if isNilReviewer(r) {
		return nil, errors.New("local round: no reviewer")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, err := r.ReviewBead(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return v.Findings, nil
}

// isNilReviewer reports whether r is missing, including a nil value of a
// nilable type held in the interface: r == nil misses those, and a method call
// on one can dereference a nil receiver.
func isNilReviewer(r BeadReviewer) bool {
	if r == nil {
		return true
	}
	switch v := reflect.ValueOf(r); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return v.IsNil()
	}
	return false
}
