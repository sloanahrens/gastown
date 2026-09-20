package refinery

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/rig"
)

// Tests for gt-bsmp: a gate rejection must close the MR wisp with a
// "rejected: ..." close_reason, so a rejected diff cannot stay
// merge-eligible. Before this, the full-gate path (gt mq reject) closed the
// wisp and the automatic paths (batch editorial review, the editorial push
// precondition, a mechanical gate verdict) did not — the wisp stayed ready,
// was re-rehearsed and re-reviewed on an unchanged head every cycle, and a
// re-roll that came back approve could land a diff a reviewer had rejected.

// reviewGateStub is an editorialExec that answers each MR with a fixed
// verdict, so a batch review can be driven without om.
func reviewGateStub(t *testing.T, verdicts map[string]string) editorial.ExecFunc {
	t.Helper()
	return func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
		verdict := verdicts[mrIDFromArgs(args)]
		if verdict == "" {
			verdict = "approve"
		}
		data, _ := json.Marshal(map[string]interface{}{"score": 0.5, "verdict": verdict})
		if err := os.WriteFile(verdictPathFromArgs(args), data, 0644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}
}

// batchRejectFixture builds two reviewable candidates whose MR beads are real
// enough to close: every bead is in the store and each MRInfo carries the
// source issue its rejection has to be reported against.
func batchRejectFixture(t *testing.T) (*Engineer, *batchReviewStore, []*MRInfo) {
	t.Helper()
	fakeBDForBatch(t)
	workDir, g, cleanup := testGitRepo(t)
	t.Cleanup(cleanup)

	ids := []string{"gt-wisp-aaa", "gt-wisp-bbb"}
	branches := map[string]string{"gt-wisp-aaa": "feature-a", "gt-wisp-bbb": "feature-b"}
	for _, id := range ids {
		createFeatureBranch(t, workDir, branches[id], id+".txt", "hello "+id+"\n")
		// editorial.Run's rehearsal reads origin/<branch>, not the local branch.
		run(t, workDir, "git", "push", "origin", branches[id])
	}
	writeEditorialManifest(t, workDir)

	var issues []*beadsdk.Issue
	for _, id := range ids {
		issues = append(issues, batchMRIssue(id, branches[id], "main", "polecats/test"))
	}
	store := newBatchReviewStore(issues...)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true, ReviewParallelism: 2}
	e.beads = beads.NewWithStore(workDir, store)

	candidates := make([]*MRInfo, 0, len(ids))
	for _, id := range ids {
		mr := makeMR(id, branches[id], "main")
		mr.Worker = "polecats/test"
		mr.SourceIssue = "gt-src-" + strings.TrimPrefix(id, "gt-wisp-")
		candidates = append(candidates, mr)
	}
	return e, store, candidates
}

// TestReviewBatchCandidates_RequestChanges_ClosesMRAndRecovers is the gt-bsmp
// core case on the batch path: a request_changes verdict must close the MR as
// rejected (not leave it queued, which is what let a rejected diff be
// re-reviewed until a re-roll came back approve) and hand the source bead to
// dead-worker recovery, since the polecat that submitted it is long gone and
// a close with no redispatch is work nobody resumes.
func TestReviewBatchCandidates_RequestChanges_ClosesMRAndRecovers(t *testing.T) {
	e, store, candidates := batchRejectFixture(t)
	e.editorialExec = reviewGateStub(t, map[string]string{"gt-wisp-bbb": "request_changes"})

	var gotReq *deadWorkerRecoveryRequest
	e.recoverDeadWorker = func(req deadWorkerRecoveryRequest) bool {
		gotReq = &req
		return true
	}

	approved, _, _ := e.reviewBatchCandidates(context.Background(), candidates, "main")

	if len(approved) != 1 || approved[0].ID != "gt-wisp-aaa" {
		t.Fatalf("expected only gt-wisp-aaa approved, got %v (output:\n%s)", mrIDs(approved), e.output)
	}

	reason := store.closeReasons["gt-wisp-bbb"]
	if reason == "" {
		t.Fatalf("request_changes candidate's MR was not closed (output:\n%s)", e.output)
	}
	if !strings.HasPrefix(reason, "rejected: EDITORIAL REJECTION") {
		t.Errorf("close_reason = %q, want a 'rejected: EDITORIAL REJECTION' reason", reason)
	}
	if !strings.Contains(reason, "request_changes") {
		t.Errorf("close_reason = %q, want it to name the verdict", reason)
	}
	if _, closed := store.closeReasons["gt-wisp-aaa"]; closed {
		t.Errorf("approved MR gt-wisp-aaa must not be closed")
	}

	if gotReq == nil {
		t.Fatal("expected dead-worker recovery for the rejected candidate")
	}
	if gotReq.SourceIssue != "gt-src-bbb" {
		t.Errorf("recovery SourceIssue = %q, want gt-src-bbb", gotReq.SourceIssue)
	}
	if gotReq.FailureType != "editorial" {
		t.Errorf("recovery FailureType = %q, want editorial", gotReq.FailureType)
	}
	if gotReq.MRID != "gt-wisp-bbb" || gotReq.Branch != "feature-b" {
		t.Errorf("recovery request names the wrong MR: %+v", gotReq)
	}
}

// TestReviewBatchCandidates_InfraExit_LeftQueued pins the other half of the
// distinction: a review that could not produce a verdict (exit 2) is not a
// rejection, so its MR must stay queued and untouched for the next cycle.
func TestReviewBatchCandidates_InfraExit_LeftQueued(t *testing.T) {
	e, store, candidates := batchRejectFixture(t)
	// First candidate: the gate script cannot run at all (exit 2, Tooling).
	e.editorialExec = func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
		if mrIDFromArgs(args) == "gt-wisp-bbb" {
			return "backend unavailable", 2, nil
		}
		data, _ := json.Marshal(map[string]interface{}{"score": 0.9, "verdict": "approve"})
		if err := os.WriteFile(verdictPathFromArgs(args), data, 0644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}

	recovered := false
	e.recoverDeadWorker = func(deadWorkerRecoveryRequest) bool {
		recovered = true
		return true
	}

	approved, reviewed, _ := e.reviewBatchCandidates(context.Background(), candidates, "main")

	if len(approved) != 1 || approved[0].ID != "gt-wisp-aaa" {
		t.Fatalf("expected only gt-wisp-aaa approved, got %v", mrIDs(approved))
	}
	if len(reviewed) != 2 {
		t.Fatalf("expected 2 reviewed entries, got %v", reviewed)
	}
	if reason, closed := store.closeReasons["gt-wisp-bbb"]; closed {
		t.Errorf("an infra-class review failure must not close the MR, got close_reason %q", reason)
	}
	if recovered {
		t.Error("an infra-class review failure must not trigger dead-worker recovery")
	}
}

// TestRejectEditorialVerdict_ClosesOnlyOnAVerdict pins the push-precondition
// side of the same rule: only verdict_not_approve — a request_changes note
// covering exactly the range being pushed — is a rejection. A missing note
// (never reviewed), a patch-id mismatch (revised since review), a version
// floor and an unresolvable range are all states the next cycle can still
// resolve, so closing on them would dequeue mergeable work.
func TestRejectEditorialVerdict_ClosesOnlyOnAVerdict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		reason    editorial.PreconditionReason
		wantClose bool
	}{
		{editorial.ReasonVerdictNotApprove, true},
		{editorial.ReasonMissing, false},
		{editorial.ReasonPatchIDMismatch, false},
		{editorial.ReasonVersionBelowMin, false},
		{editorial.ReasonRangeUnresolvable, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			workDir := t.TempDir()
			r := &rig.Rig{Name: "test-rig", Path: workDir}
			e := NewEngineer(r)
			e.output = &bytes.Buffer{}
			e.workDir = workDir

			mrIssue := prepushMRIssue("gt-wisp-mr1", "polecat/nux/gt-src1+abc", "main", "gt-src1")
			store := newPrepushStore(mrIssue)
			e.beads = beads.NewWithStore(workDir, store)

			mr := &MRInfo{ID: "gt-wisp-mr1", Branch: "polecat/nux/gt-src1+abc", Target: "main", SourceIssue: "gt-src1"}
			e.rejectEditorialVerdict(mr, &editorial.PreconditionError{Class: editorial.Precondition, MR: mr.ID, Reason: tc.reason})

			_, closed := store.closeReasons["gt-wisp-mr1"]
			if closed != tc.wantClose {
				t.Errorf("closed = %v, want %v (close_reason %q)", closed, tc.wantClose, store.closeReasons["gt-wisp-mr1"])
			}
			if tc.wantClose && !strings.HasPrefix(store.closeReasons["gt-wisp-mr1"], "rejected: ") {
				t.Errorf("close_reason = %q, want a 'rejected: ' reason", store.closeReasons["gt-wisp-mr1"])
			}
		})
	}
}

// TestHandleMRInfoFailure_GateVerdictClosesMR is the mechanical-gate half of
// gt-bsmp: a gate that ran and rejected the branch must dequeue the MR. The
// nudge and the dead-worker recovery are unchanged — the close is additive,
// because a wisp left ready is what made the rejection look retryable.
func TestHandleMRInfoFailure_GateVerdictClosesMR(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	e.output = &bytes.Buffer{}
	e.workDir = workDir

	mrIssue := prepushMRIssue("gt-wisp-mr1", "polecat/nux/gt-src1+abc", "main", "gt-src1")
	store := newPrepushStore(mrIssue)
	e.beads = beads.NewWithStore(workDir, store)
	e.recoverDeadWorker = func(deadWorkerRecoveryRequest) bool { return true }

	mr := &MRInfo{
		ID:          "gt-wisp-mr1",
		Branch:      "polecat/nux/gt-src1+abc",
		Target:      "main",
		SourceIssue: "gt-src1",
		Worker:      "polecats/nux",
	}
	e.HandleMRInfoFailure(mr, ProcessResult{
		Success:     false,
		TestsFailed: true,
		Error:       "quality gates failed: gate: exit status 1",
	})

	reason, closed := store.closeReasons["gt-wisp-mr1"]
	if !closed {
		t.Fatalf("a gate verdict must close the MR (output:\n%s)", e.output)
	}
	if !strings.HasPrefix(reason, "rejected: GATE REJECTION") {
		t.Errorf("close_reason = %q, want a 'rejected: GATE REJECTION' reason", reason)
	}
}

// TestHandleMRInfoFailure_GateInfraStaysQueued is the guard on the close: a
// gate that could not RUN (timeout, launch failure, empty command) says
// nothing about the branch, so it must leave the MR queued for retry. Closing
// on it would dequeue every MR in the queue, reopen every source bead, and
// nudge every worker over something no worker can fix.
func TestHandleMRInfoFailure_GateInfraStaysQueued(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	e.output = &bytes.Buffer{}
	e.workDir = workDir

	mrIssue := prepushMRIssue("gt-wisp-mr1", "polecat/nux/gt-src1+abc", "main", "gt-src1")
	store := newPrepushStore(mrIssue)
	e.beads = beads.NewWithStore(workDir, store)
	e.recoverDeadWorker = func(deadWorkerRecoveryRequest) bool { return true }

	mr := &MRInfo{
		ID:          "gt-wisp-mr1",
		Branch:      "polecat/nux/gt-src1+abc",
		Target:      "main",
		SourceIssue: "gt-src1",
		Worker:      "polecats/nux",
	}
	e.HandleMRInfoFailure(mr, ProcessResult{
		Success:     false,
		TestsFailed: true,
		GateInfra:   true,
		Error:       "quality gates failed: lint: timed out after 30m",
	})

	if reason, closed := store.closeReasons["gt-wisp-mr1"]; closed {
		t.Errorf("a gate that never ran must not close the MR, got close_reason %q", reason)
	}
}

// TestRunGatesForPhase_ClassifiesInfraFailures pins the classification the
// close depends on: a command that exits non-zero is a verdict on the branch,
// while an empty command, a binary that cannot be launched and a timeout are
// not. Without this the two are indistinguishable — a killed process reports
// the same *exec.ExitError a real failure does.
func TestRunGatesForPhase_ClassifiesInfraFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		cmd       string
		timeout   time.Duration
		wantInfra bool
	}{
		{name: "non-zero exit is a verdict", cmd: "exit 1", wantInfra: false},
		{name: "empty command never ran", cmd: "   ", wantInfra: true},
		{name: "unlaunchable binary never ran", cmd: "/nonexistent/gate-binary-xyz", wantInfra: true},
		{name: "timeout is not a verdict", cmd: "sleep 2", timeout: 200 * time.Millisecond, wantInfra: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Engineer{output: &bytes.Buffer{}, workDir: t.TempDir()}
			e.config = DefaultMergeQueueConfig()
			e.config.Gates = map[string]*GateConfig{
				"gate": {Cmd: tc.cmd, Timeout: tc.timeout},
			}

			result := e.runGatesForPhase(context.Background(), GatePhasePreMerge)

			if result.Success {
				t.Fatalf("gate %q unexpectedly passed", tc.cmd)
			}
			if !result.TestsFailed {
				t.Errorf("expected TestsFailed for a failing gate, got %+v", result)
			}
			if result.GateInfra != tc.wantInfra {
				t.Errorf("GateInfra = %v, want %v (error %q)", result.GateInfra, tc.wantInfra, result.Error)
			}
		})
	}
}
