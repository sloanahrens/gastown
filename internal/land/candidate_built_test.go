package land

import (
	"context"
	"testing"
)

// TestCandidateBuiltIsCalledBeforeTheGateOnTheMergedCommit: a passing landing
// announces the merge commit once, while the gate has not run on it yet.
func TestCandidateBuiltIsCalledBeforeTheGateOnTheMergedCommit(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Candidate = cand
	var calls, runsBefore int
	var got string
	l.CandidateBuilt = func(w Work, merged string) {
		calls++
		runsBefore = len(cand.calls)
		got = w.BeadID + "@" + merged
	}

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if want := f.work.BeadID + "@" + res.LandedCommit; calls != 1 || got != want {
		t.Fatalf("CandidateBuilt ran %d time(s) with %q; want once with %q", calls, got, want)
	}
	if runsBefore != 0 {
		t.Fatalf("the gate ran %d time(s) before CandidateBuilt; want none", runsBefore)
	}
}

// TestCandidateBuiltIsCalledOnceBeforeARedGate: a red gate still announces the
// candidate it gated, once, before the run that failed.
func TestCandidateBuiltIsCalledOnceBeforeARedGate(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{State: CandidateFailed, Context: "ci / gate (push)", Pushed: true}}
	l.Candidate = cand
	var calls, runsBefore int
	l.CandidateBuilt = func(Work, string) {
		calls++
		runsBefore = len(cand.calls)
	}

	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)
	if calls != 1 || runsBefore != 0 {
		t.Fatalf("CandidateBuilt ran %d time(s) with %d gate run(s) before it; want once and none", calls, runsBefore)
	}
}

// TestCandidateBuiltNilHookLandsAsBefore: with no hook set the landing gates
// and lands as it does without the hook.
func TestCandidateBuiltNilHookLandsAsBefore(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Candidate = cand

	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(cand.calls) != 1 {
		t.Fatalf("the candidate gate ran %d time(s), want once", len(cand.calls))
	}
}
