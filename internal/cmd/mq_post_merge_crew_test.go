package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
)

// The refinery clone's .githooks/pre-push lets the refinery push (and so
// delete) only polecat/* and integration/* work branches; every other
// branch belongs to its owner (gt-qyf1t).
func TestMQPostMergeRemoteBranchDeletable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		branch string
		want   bool
	}{
		{"polecat/x", true},
		{"polecat/amber/gt-e1ie+mufpu9hs", true},
		{"integration/x", true},
		{"crew/sloan/x", false},
		{"main", false},
		{"feature/x", false},
		{"beads-sync", false},
		{"polecat", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := mqPostMergeRemoteBranchDeletable(tc.branch); got != tc.want {
			t.Errorf("mqPostMergeRemoteBranchDeletable(%q) = %v, want %v", tc.branch, got, tc.want)
		}
	}
}

func testMQPostMergeCrewMR() *refinery.MergeRequest {
	mr := testMQPostMergeMR()
	mr.ID = "gt-mr-crew"
	mr.Branch = "crew/sloan/claude-7fc-unit-cycle"
	mr.Worker = "crew/sloan"
	return mr
}

func TestRunVerifiedMQPostMerge_CrewBranchLeftForOwner(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeCrewMR()}
	rigGit := &fakeMQPostMergeGit{remoteTip: mgr.mr.CommitSHA, localHead: mgr.mr.CommitSHA, deleteErr: errors.New("pre-push hook refused")}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !cleanup.LeftForOwner {
		t.Fatalf("cleanup.LeftForOwner = false, cleanup=%+v", cleanup)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote delete attempted for a crew branch: %v", rigGit.deletedBranches)
	}
	if cleanup.RemoteDeleted {
		t.Fatalf("cleanup claims a remote delete it did not do: %+v", cleanup)
	}
}

// A branch-cleanup failure after the merge landed is carried on the cleanup,
// not returned as an error: the caller must still run the rest of post-merge.
func TestResolveMQPostMerge_PostLandingDeleteFailureIsNotAnError(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{remoteTip: mgr.mr.CommitSHA, deleteErr: errors.New("failed to push some refs")}

	result, cleanup, orphan, err := resolveMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("resolveMQPostMerge error = %v, want nil after a landed merge", err)
	}
	if orphan || result == nil || result.MR == nil {
		t.Fatalf("orphan=%v result=%+v, want the MR path result", orphan, result)
	}
	if cleanup.Err == nil || !strings.Contains(cleanup.Err.Error(), "failed to push some refs") {
		t.Fatalf("cleanup.Err = %v, want the delete failure", cleanup.Err)
	}
}

// gt-1p1i: the literal "HEAD" branch refusal is still reported, but no longer
// aborts post-merge after the landing.
func TestResolveMQPostMerge_LiteralHEADBranchIsCarriedNotReturned(t *testing.T) {
	t.Parallel()
	mr := testMQPostMergeMR()
	mr.Branch = "HEAD"
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{remoteTip: mr.CommitSHA}

	result, cleanup, _, err := resolveMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, false, "")
	if err != nil {
		t.Fatalf("resolveMQPostMerge error = %v, want nil after a landed merge", err)
	}
	if result == nil {
		t.Fatal("result = nil, want the MR path result")
	}
	if cleanup.Err == nil || !strings.Contains(cleanup.Err.Error(), "gt-1p1i") {
		t.Fatalf("cleanup.Err = %v, want the gt-1p1i refusal", cleanup.Err)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote delete attempted for literal HEAD: %v", rigGit.deletedBranches)
	}
}

type mqPostMergeRunRecorder struct {
	out         bytes.Buffer
	rubric      int
	hook        int
	cycle       int
	escalations []string
}

func (r *mqPostMergeRunRecorder) deps(resolve func() (*refinery.PostMergeResult, mqPostMergeBranchCleanup, bool, error)) mqPostMergeRunDeps {
	return mqPostMergeRunDeps{
		Resolve:          resolve,
		RubricCheck:      func(*refinery.PostMergeResult, mqPostMergeBranchCleanup) { r.rubric++ },
		PostMergeCommand: func(*refinery.MergeRequest) { r.hook++ },
		UnitCycle: func(*refinery.PostMergeResult) unitCycleReport {
			r.cycle++
			return unitCycleReport{}
		},
		Escalate: func(fp, sev, msg string) {
			r.escalations = append(r.escalations, fp+"|"+sev+"|"+msg)
		},
		Out: &r.out,
	}
}

func TestRunMQPostMergeWith_CrewBranchRunsHookAndUnitCycle(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeCrewMR()}
	rigGit := &fakeMQPostMergeGit{remoteTip: mgr.mr.CommitSHA, localHead: mgr.mr.CommitSHA, deleteErr: errors.New("pre-push hook refused")}
	rigPath := t.TempDir()
	rec := &mqPostMergeRunRecorder{}

	err := runMQPostMergeWith("gastown", rec.deps(func() (*refinery.PostMergeResult, mqPostMergeBranchCleanup, bool, error) {
		return resolveMQPostMerge(mgr, rigPath, rigGit, mgr.mr.ID, false, "")
	}))
	if err != nil {
		t.Fatalf("runMQPostMergeWith: %v", err)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote delete attempted for a crew branch: %v", rigGit.deletedBranches)
	}
	want := "○ remote branch " + mgr.mr.Branch + " left for its owner (not a polecat/integration branch)"
	if !strings.Contains(rec.out.String(), want) {
		t.Fatalf("output missing %q:\n%s", want, rec.out.String())
	}
	if strings.Contains(rec.out.String(), "✗") {
		t.Fatalf("output reports a failure for a crew branch:\n%s", rec.out.String())
	}
	if rec.hook != 1 || rec.cycle != 1 || rec.rubric != 1 {
		t.Fatalf("hook=%d cycle=%d rubric=%d, want 1 each", rec.hook, rec.cycle, rec.rubric)
	}
	if len(rec.escalations) != 0 {
		t.Fatalf("escalated a crew branch: %v", rec.escalations)
	}
}

func TestRunMQPostMergeWith_PolecatDeleteFailureStillRunsHookAndUnitCycle(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{remoteTip: mgr.mr.CommitSHA, deleteErr: errors.New("failed to push some refs")}
	rigPath := t.TempDir()
	rec := &mqPostMergeRunRecorder{}

	err := runMQPostMergeWith("gastown", rec.deps(func() (*refinery.PostMergeResult, mqPostMergeBranchCleanup, bool, error) {
		return resolveMQPostMerge(mgr, rigPath, rigGit, mgr.mr.ID, false, "")
	}))
	if err != nil {
		t.Fatalf("runMQPostMergeWith error = %v, want nil: the merge landed", err)
	}
	if len(rigGit.deletedBranches) != 1 {
		t.Fatalf("remote delete attempts = %v, want one for the polecat branch", rigGit.deletedBranches)
	}
	if !strings.Contains(rec.out.String(), "✗ remote branch cleanup failed: ") || !strings.Contains(rec.out.String(), "failed to push some refs") {
		t.Fatalf("output missing the ✗ cleanup line:\n%s", rec.out.String())
	}
	if rec.hook != 1 || rec.cycle != 1 || rec.rubric != 1 {
		t.Fatalf("hook=%d cycle=%d rubric=%d, want 1 each", rec.hook, rec.cycle, rec.rubric)
	}
	if len(rec.escalations) != 1 || !strings.HasPrefix(rec.escalations[0], "post-merge-branch-cleanup:gastown|medium|") {
		t.Fatalf("escalations = %v, want one medium post-merge-branch-cleanup:gastown", rec.escalations)
	}
	if !strings.Contains(rec.escalations[0], mgr.mr.ID) || !strings.Contains(rec.escalations[0], mgr.mr.Branch) {
		t.Fatalf("escalation %q does not name the MR and branch", rec.escalations[0])
	}
}

func TestRunMQPostMergeWith_PreLandingErrorSkipsHookAndUnitCycle(t *testing.T) {
	t.Parallel()
	cases := map[string]*fakeMQPostMergeManager{
		"find failure":  {findErr: errors.New("bd unavailable")},
		"proof failure": {mr: testMQPostMergeMR()},
	}
	for name, mgr := range cases {
		t.Run(name, func(t *testing.T) {
			rigGit := &fakeMQPostMergeGit{verifyErr: errors.New("not reachable")}
			rigPath := t.TempDir()
			rec := &mqPostMergeRunRecorder{}

			err := runMQPostMergeWith("gastown", rec.deps(func() (*refinery.PostMergeResult, mqPostMergeBranchCleanup, bool, error) {
				return resolveMQPostMerge(mgr, rigPath, rigGit, "gt-mr-proof", false, "")
			}))
			if err == nil {
				t.Fatal("runMQPostMergeWith error = nil, want the pre-landing failure")
			}
			if rec.hook != 0 || rec.cycle != 0 || rec.rubric != 0 {
				t.Fatalf("hook=%d cycle=%d rubric=%d after a pre-landing failure, want 0", rec.hook, rec.cycle, rec.rubric)
			}
			if len(rec.escalations) != 0 {
				t.Fatalf("escalated a pre-landing failure: %v", rec.escalations)
			}
		})
	}
}
