package land

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// shadowLander is the fixture's lander running both gates: the candidate is
// pushed and recorded, and the local gate decides (slice 8).
func shadowLander(t *testing.T, f *landFixture, cand Candidate) *Lander {
	t.Helper()
	l := f.lander()
	l.Candidate = cand
	l.Shadow = true
	return l
}

// shadowRecord is the single landing record the fixture wrote.
func (f *landFixture) shadowRecord(t *testing.T) LandingRecord {
	t.Helper()
	lines := f.landingLines()
	if len(lines) != 1 {
		f.t.Fatalf("landings file has %d records, want one", len(lines))
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		f.t.Fatalf("decoding the landing record: %v", err)
	}
	return rec
}

// TestLandShadowRunsBothGates: a shadow rig pushes the candidate for its CI
// verdict and then runs the local gate, which is what decides and writes the
// target. Both verdicts reach the record.
func TestLandShadowRunsBothGates(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l := shadowLander(t, f, cand)
	pushed := false
	l.afterPush = func() { pushed = true }

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(cand.calls) != 1 {
		t.Fatalf("the candidate gate ran %d time(s), want once", len(cand.calls))
	}
	if len(f.gate.dirs) != 1 {
		t.Fatalf("the local gate ran %d time(s) on a shadow rig, want once", len(f.gate.dirs))
	}
	if !pushed || f.originMain() != res.LandedCommit {
		t.Fatalf("origin/main = %s; a shadow rig writes the target with the local push", f.originMain())
	}
	// The local gate is the authority, so its result is what the landing
	// carries — not the CI step the candidate gate would have recorded.
	if !res.Gate.Passed || len(res.Gate.Steps) != 1 || res.Gate.Steps[0].Name != "test" {
		t.Fatalf("result gate = %+v; want the local gate's passing result", res.Gate)
	}
	if res.CI == nil || res.CI.State != CandidatePassed {
		t.Fatalf("result CI = %+v; want the candidate's verdict recorded beside the local gate's", res.CI)
	}
	rec := f.shadowRecord(t)
	if rec.CIVerdict != CIVerdictPassed || rec.CIContext != "ci / gate (push)" ||
		rec.CIBranch != "land/gt-abc" || rec.CICandidate != res.LandedCommit {
		t.Fatalf("record %+v; want the shadow CI fields on the landed record", rec)
	}
	if !strings.Contains(rec.GateResult, "pass") {
		t.Fatalf("gate_result = %q; want the local gate's record", rec.GateResult)
	}
	if !strings.Contains(f.bead().Notes, "ci_verdict: passed") {
		t.Fatalf("the bead's landing note carries no CI verdict:\n%s", f.bead().Notes)
	}
}

// TestLandShadowCIRedIsRecordedNotReworked: a shadow rig's CI verdict never
// decides, so work CI called red still lands when the local gate passes, and
// the disagreement is in the record for the flip/no-flip call.
func TestLandShadowCIRedIsRecordedNotReworked(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	cand := &fakeCandidate{res: CandidateResult{
		State: CandidateFailed, Context: "ci / gate (push)", RunStatus: "failure", Tail: "--- FAIL: TestThing\n"}}
	l := shadowLander(t, f, cand)

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if f.originMain() != res.LandedCommit {
		t.Fatalf("origin/main = %s; want the landing to go through on a green local gate", f.originMain())
	}
	rec := f.shadowRecord(t)
	if rec.CIVerdict != CIVerdictFailed || rec.CIRunStatus != "failure" {
		t.Fatalf("record ci_verdict=%q ci_run_status=%q; want the red CI verdict recorded", rec.CIVerdict, rec.CIRunStatus)
	}
	if CountRejections(f.bead().Notes) != 0 {
		t.Fatalf("a shadow rig reworked on CI's verdict:\n%s", f.bead().Notes)
	}
}

// TestLandShadowCISilenceIsRecorded: CI reporting nothing is evidence like any
// other verdict on a shadow rig. It is recorded rather than retried, because
// the local gate is still the gate and the rig is meant to keep landing.
func TestLandShadowCISilenceIsRecorded(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	cand := &fakeCandidate{res: CandidateResult{
		State: CandidateSilent, Context: "ci / gate (push)", Err: ErrCISilence}}
	l := shadowLander(t, f, cand)

	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v; want the landing to proceed on the local gate", err)
	}
	if rec := f.shadowRecord(t); rec.CIVerdict != CIVerdictSilent {
		t.Fatalf("record ci_verdict = %q, want %q", rec.CIVerdict, CIVerdictSilent)
	}
}

// TestLandShadowLocalGateRedIsTheLocalReason: the local gate rejecting is the
// rig's own refusal, so the rework note names it and not the candidate CI
// tested but did not judge.
func TestLandShadowLocalGateRedIsTheLocalReason(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l := shadowLander(t, f, cand)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestLocal\n"}}}
	}

	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectGate, LabelRework)
	if !strings.Contains(rej.Reason, "gate failed on the merged tree") {
		t.Fatalf("reason %q; want the local gate's own failure named", rej.Reason)
	}
	if strings.Contains(rej.Reason, "candidate gate") {
		t.Fatalf("reason %q blames the candidate gate that did not decide this landing", rej.Reason)
	}
}

// TestLandShadowWithAMergerIsRefused: shadow mode's contract is the local
// write, so a lander carrying both is a misconfiguration rather than a landing
// that silently merges through the PR.
func TestLandShadowWithAMergerIsRefused(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := shadowLander(t, f, &fakeCandidate{})
	l.Merger = &fakeMerger{}

	_, err := l.Land(context.Background(), f.work)
	if err == nil || !strings.Contains(err.Error(), "Shadow") {
		t.Fatalf("Land error = %v; want the shadow/merger combination refused", err)
	}
	f.assertUntouched(t)
}

func TestCIVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		res  CandidateResult
		want string
	}{
		{"passed", CandidateResult{State: CandidatePassed}, CIVerdictPassed},
		{"failed", CandidateResult{State: CandidateFailed}, CIVerdictFailed},
		{"silence", CandidateResult{State: CandidateSilent, Err: ErrCISilence}, CIVerdictSilent},
		{"no verdict", CandidateResult{}, CIVerdictSilent},
		{"push failed", CandidateResult{Err: errors.New("pushing the candidate land/gt-abc: connection refused")},
			CIVerdictErrorPrefix + "pushing the candidate land/gt-abc: connection refused"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CIVerdict(tc.res); got != tc.want {
				t.Fatalf("CIVerdict(%+v) = %q, want %q", tc.res, got, tc.want)
			}
		})
	}
}
