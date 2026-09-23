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
// merge-eligible. Before this, `gt mq reject` and the eligibility recheck
// closed the wisp while the automatic paths — the batch editorial review and
// the editorial push precondition — did not: the wisp stayed ready, was
// re-rehearsed and re-reviewed on an unchanged head every cycle, and a re-roll
// that came back approve could land a diff a reviewer had rejected.

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
// rejected — not leave it queued, which is what let a rejected diff be
// re-reviewed until a re-roll came back approve — and hand the source bead to
// dead-worker recovery, since the polecat that submitted it exits at gt done
// and a close with no redispatch is work nobody resumes.
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
	// gt-j6ez: an editorial rejection must carry an EditorialReceipt (the om
	// score, at minimum) so gt deacon redispatch can run RedispatchEditorial
	// instead of falling back to the plain attempt-count Redispatch.
	if gotReq.Receipt == nil {
		t.Fatal("recovery request carries no Receipt — RedispatchEditorial's convergence rule can never run")
	}
	if gotReq.Receipt.Score != 0.5 {
		t.Errorf("Receipt.Score = %v, want 0.5 (the stub's verdict score)", gotReq.Receipt.Score)
	}
}

// TestReviewBatchCandidates_RequestChanges_FindingsTravelToRecovery pins that
// a rejection's full per-finding detail (not just the count) reaches the
// dead-worker recovery request, so the source bead's notes and the
// RECOVERED_BEAD mail carry the actual findings forward for the next
// reviewer (om-gate T10, gt-j6ez).
func TestReviewBatchCandidates_RequestChanges_FindingsTravelToRecovery(t *testing.T) {
	e, _, candidates := batchRejectFixture(t)
	e.editorialExec = func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
		verdict := "approve"
		var findings []map[string]interface{}
		if mrIDFromArgs(args) == "gt-wisp-bbb" {
			verdict = "request_changes"
			findings = []map[string]interface{}{
				{"id": "abc123456789", "severity": "major", "path": "internal/foo.go", "line": 42, "title": "leaky abstraction"},
			}
		}
		data, _ := json.Marshal(map[string]interface{}{"score": 0.4, "verdict": verdict, "findings": findings})
		if err := os.WriteFile(verdictPathFromArgs(args), data, 0644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}

	var gotReq *deadWorkerRecoveryRequest
	e.recoverDeadWorker = func(req deadWorkerRecoveryRequest) bool {
		gotReq = &req
		return true
	}

	e.reviewBatchCandidates(context.Background(), candidates, "main")

	if gotReq == nil {
		t.Fatal("expected dead-worker recovery for the rejected candidate")
	}
	if len(gotReq.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly 1", gotReq.Findings)
	}
	f := gotReq.Findings[0]
	if f.ID != "abc123456789" || f.Severity != "major" || f.Path != "internal/foo.go" || f.Line != 42 || f.Title != "leaky abstraction" {
		t.Errorf("Findings[0] = %+v, unexpected", f)
	}
}

// sourceBeadWithRejectionHistory is a source bead carrying n MERGE REJECTION
// blocks, as the single-MR path (mol-refinery-patrol) leaves them.
func sourceBeadWithRejectionHistory(id string, n int) *beadsdk.Issue {
	now := time.Now()
	return &beadsdk.Issue{
		ID:        id,
		Title:     id,
		Notes:     priorRejectionBlocks(n),
		Status:    beadsdk.StatusOpen,
		IssueType: beadsdk.IssueType("task"),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// TestReviewBatchCandidates_AttemptNumberFromSourceBeadHistory is gt-gld77's
// acceptance case on the batch path: a fresh MR (RetryCount 0) whose source
// bead has already been rejected three times is that bead's FOURTH attempt.
// The batch used to number the review from the MR's conflict-retry count, so it
// reviewed this bead as "Attempt 1" and wrote a second "(attempt 1)" onto it —
// gt-0wy03's markers read 1, 2, 3, 1, leaving anything that reconstructs the
// bead's history with two attempt 1s and no attempt 4.
//
// Both halves of the count are pinned here: the number the review records, and
// the number the rejection note's header and close reason carry.
func TestReviewBatchCandidates_AttemptNumberFromSourceBeadHistory(t *testing.T) {
	e, store, candidates := batchRejectFixture(t)
	store.issues["gt-src-aaa"] = sourceBeadWithRejectionHistory("gt-src-aaa", 1)
	store.issues["gt-src-bbb"] = sourceBeadWithRejectionHistory("gt-src-bbb", 3)
	e.editorialExec = reviewGateStub(t, map[string]string{"gt-wisp-bbb": "request_changes"})

	var gotReq *deadWorkerRecoveryRequest
	e.recoverDeadWorker = func(req deadWorkerRecoveryRequest) bool {
		gotReq = &req
		return true
	}

	_, _, notes := e.reviewBatchCandidates(context.Background(), candidates, "main")

	// gt-src-aaa carries one prior rejection, so its review is attempt 2.
	if note := notes["gt-wisp-aaa"]; note == nil || note.Attempt != 2 {
		t.Errorf("gt-wisp-aaa reviewed as %v, want attempt 2 (one prior rejection on gt-src-aaa)", note)
	}

	// gt-src-bbb carries three, so its review — and the rejection that
	// follows it — is attempt 4.
	if gotReq == nil {
		t.Fatal("expected dead-worker recovery for the rejected candidate")
	}
	if gotReq.AttemptNumber != 4 {
		t.Errorf("recovery AttemptNumber = %d, want 4 (three prior rejections on gt-src-bbb)", gotReq.AttemptNumber)
	}
	reason := store.closeReasons["gt-wisp-bbb"]
	if !strings.Contains(reason, "EDITORIAL REJECTION (attempt 4)") {
		t.Errorf("close_reason = %q, want it to name attempt 4", reason)
	}

	// The note the rejection leaves behind is this request through the shared
	// formatter: header number and MR id, so a reader can tell attempts apart
	// even where numbers legitimately collide.
	note := formatMergeRejectionNote(*gotReq)
	if !strings.Contains(note, "MERGE REJECTION (attempt 4):") {
		t.Errorf("rejection note header does not name attempt 4:\n%s", note)
	}
	if !strings.Contains(note, "MR: gt-wisp-bbb") {
		t.Errorf("rejection note does not carry the MR id:\n%s", note)
	}
}

// TestReviewBatchCandidates_InfraExit_LeftQueued pins the other half of the
// distinction: a review that could not produce a verdict (exit 2) is not a
// rejection, so its MR must stay queued and untouched for the next cycle.
func TestReviewBatchCandidates_InfraExit_LeftQueued(t *testing.T) {
	e, store, candidates := batchRejectFixture(t)
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
