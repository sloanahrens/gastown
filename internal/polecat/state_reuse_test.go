package polecat

import "testing"

// TestStateIsReuseEligible pins the single predicate the allocator
// (FindIdlePolecat) and the reporting path (DecideWorkstate) must share.
// Regression for gt-uu6: FindIdlePolecat demanded StateIdle while
// DecideWorkstate already admitted StateDone, so 'gt polecat list' showed a
// reusable pool that 'gt sling' could never allocate from and every spawn
// fell through to the per-rig directory cap.
func TestStateIsReuseEligible(t *testing.T) {
	cases := []struct {
		state State
		want  bool
	}{
		{StateIdle, true},
		{StateDone, true},
		{StateWorking, false},
		{StateStalled, false},
		{StateReviewNeeded, false},
		{State(""), false},
	}
	for _, tc := range cases {
		if got := tc.state.IsReuseEligible(); got != tc.want {
			t.Errorf("State(%q).IsReuseEligible() = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// TestDecideWorkstateAgreesWithReuseEligibility guards the other half of the
// asymmetry: every state DecideWorkstate refuses to evaluate for reuse must be
// exactly the set IsReuseEligible rejects, and a clean StateDone polecat must
// come out Reusable so the allocator's candidate filter and the verdict agree.
func TestDecideWorkstateAgreesWithReuseEligibility(t *testing.T) {
	for _, s := range []State{StateIdle, StateDone, StateWorking, StateStalled, StateReviewNeeded} {
		d := DecideWorkstate(WorkstateInput{State: s, CleanupStatus: CleanupClean})
		if s.IsReuseEligible() != d.Reusable {
			t.Errorf("State(%q): IsReuseEligible()=%v but DecideWorkstate(clean).Reusable=%v", s, s.IsReuseEligible(), d.Reusable)
		}
	}
}
