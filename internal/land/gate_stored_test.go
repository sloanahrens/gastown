package land

import (
	"context"
	"errors"
	"testing"
)

// landCarStored lands w through the stages Land runs, with stored in the car
// the gate step is given (gt-kbzz2i); production sets no slot yet.
func landCarStored(t *testing.T, l *Lander, w Work, stored *CandidateResult) (Result, error) {
	t.Helper()
	ctx := context.Background()
	p, res, err := l.precheckCar(ctx, w)
	if p == nil {
		return res, err
	}
	c, cleanup, res, err := l.mergeCar(ctx, p.g, p.issue, w, p.base, p.storedConflict, 0)
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	if c == nil {
		return res, err
	}
	ctx = WithLandingID(ctx, c.landingID)
	c.w.CandidateBranch, c.w.CandidateHead = c.w.Candidate(), c.merged
	rv, err := l.reviewCar(ctx, c)
	if err != nil {
		return Result{}, err
	}
	c.stored = stored
	gr, err := l.gateCar(ctx, c, rv)
	if err != nil {
		return Result{}, err
	}
	res = Result{LandedCommit: c.merged, PatchID: c.patchID, Base: p.base, Verdict: rv.verdict, Gate: gr.res}
	return l.finishCar(ctx, c, res, rv.verdict, gr.ciContext, gr.cand)
}

// gateEnd is how a landing ended and what its candidate and hooks saw.
type gateEnd struct {
	end                             string
	runs, discards, hook, announced int
}

// runGate lands one car whose candidate gate result is res, from the stored
// slot when stored is set and from a candidate run otherwise.
func runGate(t *testing.T, res CandidateResult, stored bool) gateEnd {
	t.Helper()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: res}
	l.Candidate = cand
	var out gateEnd
	l.CandidateBuilt = func(Work, string) { out.announced++ }
	l.CIFailure = func(CIFailure) { out.hook++ }
	var slot *CandidateResult
	if stored {
		slot = &res
	}
	_, err := landCarStored(t, l, f.work, slot)
	out.end, out.runs, out.discards = endOf(err), len(cand.calls), len(cand.discarded)
	return out
}

// endOf names how a landing ended: landed, a refusal by kind, an infra error
// for no verdict, or any other infra error.
func endOf(err error) string {
	var rej *Rejection
	switch {
	case err == nil:
		return "landed"
	case errors.As(err, &rej):
		return "rejected: " + string(rej.Kind)
	case errors.Is(err, ErrCISilence):
		return "infra: silent"
	}
	return "infra"
}

// TestGateStoredEndsAsARunDoes: a car carrying a passed, red, silent or erroring
// result ends its landing as the same result from a run does. A stored result
// runs no candidate gate and announces nothing; a run does both.
func TestGateStoredEndsAsARunDoes(t *testing.T) {
	t.Parallel()
	const ctx = "ci / gate (push)"
	tests := []struct {
		name string
		res  CandidateResult
		want gateEnd
	}{
		{"passed", CandidateResult{State: CandidatePassed, Context: ctx, Pushed: true}, gateEnd{end: "landed"}},
		{"red", CandidateResult{State: CandidateFailed, Context: ctx, Pushed: true, Tail: "--- FAIL: TestThing\n"}, gateEnd{end: "rejected: gate", discards: 1, hook: 1}},
		{"silent", CandidateResult{State: CandidateSilent, Context: ctx, Pushed: true}, gateEnd{end: "infra: silent", discards: 1}},
		{"erroring", CandidateResult{State: CandidatePassed, Context: ctx, Pushed: true, Err: errors.New("container slot unavailable")}, gateEnd{end: "infra", discards: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runGate(t, tc.res, true); got != tc.want {
				t.Fatalf("stored landing = %+v, want %+v", got, tc.want)
			}
			wantRun := tc.want
			wantRun.runs, wantRun.announced = 1, 1
			if got := runGate(t, tc.res, false); got != wantRun {
				t.Fatalf("run landing = %+v, want %+v", got, wantRun)
			}
		})
	}
}
