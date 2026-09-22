package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
)

type fakeMQPostMergeManager struct {
	mr              *refinery.MergeRequest
	findErr         error
	postMergeErr    error
	postMergeCalled bool
	postMergeMR     *refinery.MergeRequest
}

func (m *fakeMQPostMergeManager) FindMRForPostMerge(string) (*refinery.MergeRequest, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	return m.mr, nil
}

func (m *fakeMQPostMergeManager) PostMergeMR(mr *refinery.MergeRequest) (*refinery.PostMergeResult, error) {
	m.postMergeCalled = true
	m.postMergeMR = mr
	if m.postMergeErr != nil {
		return nil, m.postMergeErr
	}
	return &refinery.PostMergeResult{MR: m.mr, MRClosed: true, SourceIssueClosed: true, SourceIssueID: m.mr.IssueID}, nil
}

type fakeMQPostMergeGit struct {
	verifyErr error
	openPR    bool
	deleteErr error
	remoteTip string
	localHead string
	tipErr    error

	// Attestation-binding fixtures (verifyLandedCommitMatchesSubmitted).
	mergeBase       string
	mergeBaseErr    error
	landedParent    string
	landedParentErr error
	submittedFiles  []string
	landedFiles     []string
	diffErr         error

	// resolvedCommit is what Rev returns for an attested commit, modelling git's
	// abbreviated-SHA expansion (resolveMQPostMergeCommit).
	resolvedCommit string
	resolveErr     error

	// Orphan-branch cleanup fixtures (runOrphanMQPostMerge).
	defaultBranch string
	targetRef     string
	pruneErr      error
	preserved     bool
	unpreserved   int
	preserveErr   error

	verifiedCommits  []string
	deletedBranches  []string
	deletedHeads     []string
	localDeleted     []string
	prunedRemotes    []string
	preservedAgainst []string
	targetRefs       []string
	mergeBaseSubmits []string
}

func (g *fakeMQPostMergeGit) VerifyPushedCommitReachableFromPushTarget(_, _, commit string) error {
	g.verifiedCommits = append(g.verifiedCommits, commit)
	return g.verifyErr
}

func (g *fakeMQPostMergeGit) PushRemoteRefTargetStatus(_ string, ref git.RemoteRef, target string) (git.BranchPreservationStatus, error) {
	g.preservedAgainst = append(g.preservedAgainst, ref.Name)
	g.targetRefs = append(g.targetRefs, target)
	if g.preserveErr != nil {
		return git.BranchPreservationStatus{}, g.preserveErr
	}
	return git.BranchPreservationStatus{Preserved: g.preserved, UnpreservedPatchCount: g.unpreserved}, nil
}

func (g *fakeMQPostMergeGit) RemoteDefaultBranch() string {
	if g.defaultBranch == "" {
		return "main"
	}
	return g.defaultBranch
}

func (g *fakeMQPostMergeGit) CleanDefaultBranchBaseRef(remote, defaultBranch string) string {
	if g.targetRef != "" {
		return g.targetRef
	}
	return remote + "/" + defaultBranch
}

func (g *fakeMQPostMergeGit) CleanBaseRef(remote, defaultBranch, target string) string {
	if target == "" || target == defaultBranch {
		return g.CleanDefaultBranchBaseRef(remote, defaultBranch)
	}
	return remote + "/" + target
}

func (g *fakeMQPostMergeGit) FetchPrune(remote string) error {
	g.prunedRemotes = append(g.prunedRemotes, remote)
	return g.pruneErr
}

func (g *fakeMQPostMergeGit) HasOpenPullRequest(git.PullRequestRef) bool {
	return g.openPR
}

func (g *fakeMQPostMergeGit) PushRemoteBranchTip(_, _ string) (string, error) {
	return g.remoteTip, g.tipErr
}

func (g *fakeMQPostMergeGit) Rev(ref string) (string, error) {
	if resolved, ok := strings.CutSuffix(ref, "^{commit}"); ok {
		if strings.HasPrefix(resolved, "refs/") {
			// Peel a local branch ref (deleteMQPostMergeLocalBranchIfAt): the
			// answer is that branch's head.
			return g.localHead, nil
		}
		// Expand an abbreviated attestation to a full SHA
		// (resolveMQPostMergeCommit). Absent a fixture, echo the SHA back so
		// tests that already pass a full SHA see it unchanged.
		if g.resolveErr != nil {
			return "", g.resolveErr
		}
		if g.resolvedCommit != "" {
			return g.resolvedCommit, nil
		}
		return resolved, nil
	}
	if strings.HasSuffix(ref, "^") {
		return g.landedParent, g.landedParentErr
	}
	return g.localHead, nil
}

func (g *fakeMQPostMergeGit) MergeBase(_, submittedCommit string) (string, error) {
	g.mergeBaseSubmits = append(g.mergeBaseSubmits, submittedCommit)
	return g.mergeBase, g.mergeBaseErr
}

// DiffNameOnly returns g.submittedFiles when asked for the submitted range
// (base == g.mergeBase, as produced by MergeBase above) and g.landedFiles
// for the attested range (base == g.landedParent, as produced by Rev above).
func (g *fakeMQPostMergeGit) DiffNameOnly(base, _ string) ([]string, error) {
	if g.diffErr != nil {
		return nil, g.diffErr
	}
	if base == g.landedParent && g.landedParent != "" {
		return g.landedFiles, nil
	}
	return g.submittedFiles, nil
}

func (g *fakeMQPostMergeGit) DeleteRemoteBranchIfAt(_, branch, expectedHash string) error {
	g.deletedBranches = append(g.deletedBranches, branch)
	g.deletedHeads = append(g.deletedHeads, expectedHash)
	return g.deleteErr
}

func (g *fakeMQPostMergeGit) DeleteBranch(branch string, _ bool) error {
	g.localDeleted = append(g.localDeleted, branch)
	return nil
}

func testMQPostMergeMR() *refinery.MergeRequest {
	return &refinery.MergeRequest{
		ID:           "gt-mr-proof",
		Branch:       "polecat/test/gt-proof",
		Worker:       "polecats/test",
		IssueID:      "gt-proof",
		TargetBranch: "main",
		CommitSHA:    "abc123def456",
	}
}

func TestRunVerifiedMQPostMerge_ProofFailurePreservesRecordsAndBranch(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{verifyErr: errors.New("not reachable")}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "merge proof failed") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want merge proof failure", err)
	}
	if !strings.Contains(err.Error(), mgr.mr.CommitSHA) {
		t.Fatalf("proof error %q does not mention submitted head %s", err, mgr.mr.CommitSHA)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called after failed proof")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote branch deleted after failed proof: %v", rigGit.deletedBranches)
	}
	if len(rigGit.localDeleted) != 0 {
		t.Fatalf("local branch deleted after failed proof: %v", rigGit.localDeleted)
	}
}

// TestRunVerifiedMQPostMerge_LandedCommitAttestationSatisfiesConflictResolvedRebase
// covers the sequential-rebase-with-conflict-resolution case: the submitted
// commit_sha's patch-id no longer matches anything on target (conflict
// resolution legitimately changed it), so the default proof fails, but an
// explicit --landed-commit attestation for the SHA the refinery actually
// pushed satisfies the proof instead (gt-f5f6).
func TestRunVerifiedMQPostMerge_LandedCommitAttestationSatisfiesConflictResolvedRebase(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{
		mergeBase:      "oldbase000",
		landedParent:   "oldmain111",
		submittedFiles: []string{"internal/cmd/mq.go"},
		// Conflict resolution touched an extra file and changed hunks within
		// the shared one — still a superset of what the polecat submitted.
		landedFiles: []string{"internal/cmd/mq.go", "internal/cmd/mq_integration.go"},
	}
	const landedCommit = "2bb0bf7f2bb0bf7f2bb0bf7f2bb0bf7f2bb0bf7f"

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, landedCommit)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after attested proof")
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != landedCommit {
		t.Fatalf("verified commits = %v, want [%s] (attested landed commit, not submitted head)", rigGit.verifiedCommits, landedCommit)
	}
	if mgr.postMergeMR.MergeCommit != landedCommit {
		t.Fatalf("MR MergeCommit = %q, want attested landed commit %q recorded for close", mgr.postMergeMR.MergeCommit, landedCommit)
	}
}

// TestRunVerifiedMQPostMerge_LandedCommitAttestationStillVerified ensures a
// bogus or stale --landed-commit is rejected rather than trusted blindly —
// it must itself be reachable from target.
func TestRunVerifiedMQPostMerge_LandedCommitAttestationStillVerified(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{verifyErr: errors.New("not reachable")}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "0000000000000000000000000000000000000f")
	if err == nil || !strings.Contains(err.Error(), "attested landed commit") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want attested-landed-commit failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called after failed attestation")
	}
}

// TestRunVerifiedMQPostMerge_LandedCommitAttestationRejectsUnrelatedCommit
// covers the reported vulnerability (gt-7dnx): --landed-commit previously
// only had to be *reachable* from target, which every commit already on
// target trivially is. An unrelated on-target commit — reachable, but whose
// changed files share nothing with what the MR actually submitted — must be
// rejected rather than accepted as proof this MR landed.
func TestRunVerifiedMQPostMerge_LandedCommitAttestationRejectsUnrelatedCommit(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{
		mergeBase:      "oldbase000",
		landedParent:   "oldmain111",
		submittedFiles: []string{"internal/cmd/mq.go"},
		// An unrelated commit that happens to already be on target: it never
		// touched the file the submitted MR changed.
		landedFiles: []string{"docs/unrelated.md"},
	}
	const unrelatedOnTargetCommit = "cafef00dcafef00dcafef00dcafef00dcafef00d"

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, unrelatedOnTargetCommit)
	if err == nil || !strings.Contains(err.Error(), "attestation does not match MR") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want attestation-does-not-match-MR failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called after unrelated-commit attestation")
	}
	if mgr.postMergeMR != nil && mgr.postMergeMR.MergeCommit == unrelatedOnTargetCommit {
		t.Fatal("MergeCommit recorded for an attestation that did not match the MR")
	}
}

// TestRunVerifiedMQPostMerge_LandedCommitAttestationUsesLiveBranchHeadWhenCommitSHAIsStale
// covers the reported incident (gt-lk6g): a conflict-resolution push
// advanced the source branch after submission without the MR bead's
// commit_sha ever being updated. Binding the attestation to that stale
// commit_sha compared it against content that no longer reflected the
// branch's real work; here the fix must resolve the branch's live remote tip
// and bind the attestation to that instead.
func TestRunVerifiedMQPostMerge_LandedCommitAttestationUsesLiveBranchHeadWhenCommitSHAIsStale(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const liveHead = "1089ca701089ca701089ca701089ca701089ca70"
	rigGit := &fakeMQPostMergeGit{
		remoteTip:      liveHead,
		mergeBase:      "oldbase000",
		landedParent:   "oldmain111",
		submittedFiles: []string{"internal/cmd/mq.go"},
		landedFiles:    []string{"internal/cmd/mq.go"},
	}
	const landedCommit = "80b0bcd80b0bcd80b0bcd80b0bcd80b0bcd80b0"

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, landedCommit)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after attested proof")
	}
	if len(rigGit.mergeBaseSubmits) != 1 || rigGit.mergeBaseSubmits[0] != liveHead {
		t.Fatalf("attestation bound against %v, want the live branch tip [%s] instead of the stale commit_sha %s", rigGit.mergeBaseSubmits, liveHead, mgr.mr.CommitSHA)
	}
}

// TestRunVerifiedMQPostMerge_AttestationAcceptedWhenSubmittedHeadAlreadyLanded
// covers the case the review flagged as still broken (gt-mlla): the submitted
// head is already reachable from target, so the merge-base of the two IS the
// submitted head and the submitted range is empty. That is a redundant
// attestation — a retry after the branch was already fast-forwarded, or an
// operator passing the flag out of habit — not a wrong one, and the default
// proof would have accepted the MR with no flag at all. It must be accepted,
// and the recorded merge_commit must be the commit that demonstrably landed
// rather than the unverified attestation.
func TestRunVerifiedMQPostMerge_AttestationAcceptedWhenSubmittedHeadAlreadyLanded(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const liveHead = "1a2b3c4d1a2b3c4d1a2b3c4d1a2b3c4d1a2b3c4d"
	rigGit := &fakeMQPostMergeGit{
		remoteTip: liveHead,
		// MergeBase(target, submittedHead) == submittedHead: the head is an
		// ancestor of target, so there is no diverged range to bind against.
		mergeBase: liveHead,
	}
	const redundantAttestation = "9f8e7d6c9f8e7d6c9f8e7d6c9f8e7d6c9f8e7d6c"

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, redundantAttestation)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called: a redundant attestation blocked a merge whose work had landed")
	}
	if mgr.postMergeMR.MergeCommit != liveHead {
		t.Fatalf("MR MergeCommit = %q, want the submitted head %q that is actually on target — not the unverified attestation %q",
			mgr.postMergeMR.MergeCommit, liveHead, redundantAttestation)
	}
}

// TestRunVerifiedMQPostMerge_AttestationPersistsFullSHA pins the minor the
// review raised alongside the binding (gt-mlla): an abbreviate attestation
// used to be written onto the MR bead verbatim, so the closed MR carried a
// merge_commit that could no longer be resolved once the branch was deleted.
func TestRunVerifiedMQPostMerge_AttestationPersistsFullSHA(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const fullSHA = "2bb0bf7f2bb0bf7f2bb0bf7f2bb0bf7f2bb0bf7f"
	rigGit := &fakeMQPostMergeGit{
		mergeBase:      "oldbase000",
		landedParent:   "oldmain111",
		submittedFiles: []string{"internal/cmd/mq.go"},
		landedFiles:    []string{"internal/cmd/mq.go"},
		resolvedCommit: fullSHA,
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, "2bb0bf7")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if mgr.postMergeMR.MergeCommit != fullSHA {
		t.Fatalf("MR MergeCommit = %q, want the resolved full SHA %q", mgr.postMergeMR.MergeCommit, fullSHA)
	}
}

// TestRunVerifiedMQPostMerge_AttestationWithBranchDeletionEnabled covers the
// untested interaction the review called out (gt-mlla): the attestation path
// was only ever exercised with --skip-branch-delete, so nothing pinned that an
// attested merge still deletes the branch it closed.
func TestRunVerifiedMQPostMerge_AttestationWithBranchDeletionEnabled(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{
		// Branch unmoved, so the CAS delete pins at the MR's recorded head.
		remoteTip:      mgr.mr.CommitSHA,
		localHead:      mgr.mr.CommitSHA,
		mergeBase:      "oldbase000",
		landedParent:   "oldmain111",
		submittedFiles: []string{"internal/cmd/mq.go"},
		landedFiles:    []string{"internal/cmd/mq.go", "internal/cmd/mq_status.go"},
	}
	const landedCommit = "3aba5e1f3aba5e1f3aba5e1f3aba5e1f3aba5e1f"

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, landedCommit)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the merged branch deleted after an attested merge", cleanup)
	}
	if !cleanup.LocalDeleted {
		t.Fatalf("cleanup = %+v, want the local branch deleted too", cleanup)
	}
	if len(rigGit.deletedBranches) != 1 || rigGit.deletedBranches[0] != mgr.mr.Branch {
		t.Fatalf("deleted branches = %v, want [%s]", rigGit.deletedBranches, mgr.mr.Branch)
	}
	// The delete pins to the MR's recorded head — which conflict resolution
	// refreshes to the resolved head — not to the attestation, which names the
	// commit the refinery pushed to the target.
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != mgr.mr.CommitSHA {
		t.Fatalf("deleted heads = %v, want the MR's recorded head [%s]", rigGit.deletedHeads, mgr.mr.CommitSHA)
	}
	if mgr.postMergeMR.MergeCommit != landedCommit {
		t.Fatalf("MR MergeCommit = %q, want %q", mgr.postMergeMR.MergeCommit, landedCommit)
	}
}

func TestRunVerifiedMQPostMerge_VerifiedHeadClosesAndLeaseDeletes(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{remoteTip: mgr.mr.CommitSHA, localHead: mgr.mr.CommitSHA}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if mgr.postMergeMR != mgr.mr {
		t.Fatal("PostMerge did not use the verified MR snapshot")
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != mgr.mr.CommitSHA {
		t.Fatalf("verified commits = %v, want [%s]", rigGit.verifiedCommits, mgr.mr.CommitSHA)
	}
	if !cleanup.RemoteDeleted || len(rigGit.deletedBranches) != 1 || rigGit.deletedBranches[0] != mgr.mr.Branch {
		t.Fatalf("remote delete = cleanup=%+v branches=%v", cleanup, rigGit.deletedBranches)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != mgr.mr.CommitSHA {
		t.Fatalf("deleted heads = %v, want [%s]", rigGit.deletedHeads, mgr.mr.CommitSHA)
	}
	if !cleanup.LocalDeleted || len(rigGit.localDeleted) != 1 || rigGit.localDeleted[0] != mgr.mr.Branch {
		t.Fatalf("local delete = cleanup=%+v local=%v", cleanup, rigGit.localDeleted)
	}
}

// TestRunVerifiedMQPostMerge_MovedBranchTipDeletesAtLiveHeadWhenPreserved
// covers the other half of the reported incident (gt-lk6g): the remote tip
// moved past the submitted commit_sha (a conflict-resolution push that never
// updated the MR bead), so the CAS delete pinned to the stale commit_sha is
// rejected by the remote as "stale info" even though the branch's current
// content is preserved on target. The delete must pin to the live tip
// instead, once that tip is proven preserved.
func TestRunVerifiedMQPostMerge_MovedBranchTipDeletesAtLiveHeadWhenPreserved(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const movedHead = "1089ca701089ca701089ca701089ca701089ca70"
	rigGit := &fakeMQPostMergeGit{remoteTip: movedHead, localHead: movedHead, preserved: true}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if len(rigGit.prunedRemotes) != 1 || rigGit.prunedRemotes[0] != "origin" {
		t.Fatalf("pruned remotes = %v, want [origin] (the comparison ref must be current)", rigGit.prunedRemotes)
	}
	if len(rigGit.preservedAgainst) != 1 || rigGit.preservedAgainst[0] != "refs/heads/"+mgr.mr.Branch {
		t.Fatalf("preservation checked for %v, want [refs/heads/%s]", rigGit.preservedAgainst, mgr.mr.Branch)
	}
	if len(rigGit.targetRefs) != 1 || rigGit.targetRefs[0] != "origin/main" {
		t.Fatalf("preservation targets = %v, want [origin/main]", rigGit.targetRefs)
	}
	if !cleanup.RemoteDeleted || len(rigGit.deletedBranches) != 1 || rigGit.deletedBranches[0] != mgr.mr.Branch {
		t.Fatalf("remote delete = cleanup=%+v branches=%v", cleanup, rigGit.deletedBranches)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != movedHead {
		t.Fatalf("deleted heads = %v, want [%s] (the live tip, not the stale commit_sha %s)", rigGit.deletedHeads, movedHead, mgr.mr.CommitSHA)
	}
	if !cleanup.LocalDeleted || len(rigGit.localDeleted) != 1 || rigGit.localDeleted[0] != mgr.mr.Branch {
		t.Fatalf("local delete = cleanup=%+v local=%v", cleanup, rigGit.localDeleted)
	}
}

// TestRunVerifiedMQPostMerge_MovedBranchTipRefusedWhenNotPreserved keeps the
// moved-tip adoption from becoming a way to delete a branch that picked up
// unverified commits after submission: only a live tip proven preserved on
// target is trusted as the delete anchor.
func TestRunVerifiedMQPostMerge_MovedBranchTipRefusedWhenNotPreserved(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const movedHead = "1089ca701089ca701089ca701089ca701089ca70"
	rigGit := &fakeMQPostMergeGit{remoteTip: movedHead, preserved: false, unpreserved: 2}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "not preserved on origin/main") || !strings.Contains(err.Error(), "2 patch-unique commits") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want unpreserved-tip refusal", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted despite an unpreserved moved tip: %v", rigGit.deletedBranches)
	}
	if cleanup.RemoteDeleted || cleanup.LocalDeleted {
		t.Fatalf("cleanup claims a delete it did not do: %+v", cleanup)
	}
}

func TestRunVerifiedMQPostMerge_SkipBranchDeleteStillRequiresProof(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != mgr.mr.CommitSHA {
		t.Fatalf("verified commits = %v, want [%s]", rigGit.verifiedCommits, mgr.mr.CommitSHA)
	}
	if !cleanup.Skipped {
		t.Fatalf("cleanup.Skipped = false, cleanup=%+v", cleanup)
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted despite skip: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_OpenPRSkipsRemoteDeleteAfterProof(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{openPR: true, localHead: mgr.mr.CommitSHA}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if !cleanup.OpenPR {
		t.Fatalf("cleanup.OpenPR = false, cleanup=%+v", cleanup)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote branch deleted despite open PR: %v", rigGit.deletedBranches)
	}
	if len(rigGit.localDeleted) != 1 || rigGit.localDeleted[0] != mgr.mr.Branch {
		t.Fatalf("local branch cleanup = %v, want [%s]", rigGit.localDeleted, mgr.mr.Branch)
	}
}

func TestRunVerifiedMQPostMerge_LeaseDeleteFailureReturnsAfterPostMerge(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{remoteTip: mgr.mr.CommitSHA, deleteErr: errors.New("stale info")}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "remote branch delete") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want remote branch delete failure", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if len(rigGit.deletedBranches) != 1 || rigGit.deletedBranches[0] != mgr.mr.Branch {
		t.Fatalf("remote delete attempts = %v, want [%s]", rigGit.deletedBranches, mgr.mr.Branch)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != mgr.mr.CommitSHA {
		t.Fatalf("delete lease heads = %v, want [%s]", rigGit.deletedHeads, mgr.mr.CommitSHA)
	}
	if len(rigGit.localDeleted) != 0 {
		t.Fatalf("local branch deleted after remote lease failure: %v", rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_MissingRemoteBranchIsIdempotentAfterProof(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{localHead: mgr.mr.CommitSHA}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if !cleanup.AlreadyGone {
		t.Fatalf("cleanup.AlreadyGone = false, cleanup=%+v", cleanup)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote branch delete attempted for missing branch: %v", rigGit.deletedBranches)
	}
}

func TestRunVerifiedMQPostMerge_MissingSubmittedHeadFailsClosed(t *testing.T) {
	t.Parallel()
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "missing submitted commit_sha") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want missing submitted head", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called with missing submitted head")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted with missing submitted head: %v", rigGit.deletedBranches)
	}
}

func TestRunVerifiedMQPostMerge_SourceTargetBranchFailsClosed(t *testing.T) {
	t.Parallel()
	mr := testMQPostMergeMR()
	mr.Branch = "main"
	mr.TargetBranch = "main"
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "matches target branch") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want source/target failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called when source branch matched target")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted when source matched target: %v", rigGit.deletedBranches)
	}
}

// --- Orphaned-branch cleanup (gt-qjp2) ---------------------------------------
//
// A branch whose MR bead is gone has no commit_sha, PR identity, or source
// issue to read, so the orphan path proves the delete against the branch's own
// live remote tip instead.

const testMQOrphanBranch = "polecat/test/gt-orphan"
const testMQOrphanTip = "0ddball0ddball0ddball0ddball0ddball0dd"

func testMQOrphanGit() *fakeMQPostMergeGit {
	return &fakeMQPostMergeGit{preserved: true, remoteTip: testMQOrphanTip, localHead: testMQOrphanTip}
}

func TestRunOrphanMQPostMerge_DeletesBranchPreservedOnTarget(t *testing.T) {
	t.Parallel()
	rigGit := testMQOrphanGit()

	cleanup, err := runOrphanMQPostMerge(t.TempDir(), rigGit, testMQOrphanBranch, false, "")
	if err != nil {
		t.Fatalf("runOrphanMQPostMerge: %v", err)
	}
	if cleanup.Target != "origin/main" {
		t.Fatalf("cleanup.Target = %q, want origin/main", cleanup.Target)
	}
	if len(rigGit.prunedRemotes) != 1 || rigGit.prunedRemotes[0] != "origin" {
		t.Fatalf("pruned remotes = %v, want [origin] (the comparison ref must be current)", rigGit.prunedRemotes)
	}
	if len(rigGit.preservedAgainst) != 1 || rigGit.preservedAgainst[0] != "refs/heads/"+testMQOrphanBranch {
		t.Fatalf("preservation checked for %v, want [refs/heads/%s]", rigGit.preservedAgainst, testMQOrphanBranch)
	}
	if len(rigGit.targetRefs) != 1 || rigGit.targetRefs[0] != "origin/main" {
		t.Fatalf("preservation targets = %v, want [origin/main]", rigGit.targetRefs)
	}
	if !cleanup.RemoteDeleted || len(rigGit.deletedBranches) != 1 || rigGit.deletedBranches[0] != testMQOrphanBranch {
		t.Fatalf("remote delete = cleanup=%+v branches=%v", cleanup, rigGit.deletedBranches)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != testMQOrphanTip {
		t.Fatalf("deleted heads = %v, want [%s] (the tip that was verified)", rigGit.deletedHeads, testMQOrphanTip)
	}
	if !cleanup.LocalDeleted || len(rigGit.localDeleted) != 1 {
		t.Fatalf("local delete = cleanup=%+v local=%v", cleanup, rigGit.localDeleted)
	}
}

func TestRunOrphanMQPostMerge_RefusesBranchNotPreservedOnTarget(t *testing.T) {
	t.Parallel()
	rigGit := testMQOrphanGit()
	rigGit.preserved = false
	rigGit.unpreserved = 3

	cleanup, err := runOrphanMQPostMerge(t.TempDir(), rigGit, testMQOrphanBranch, false, "")
	if err == nil || !strings.Contains(err.Error(), "not preserved on origin/main") || !strings.Contains(err.Error(), "3 patch-unique commits") {
		t.Fatalf("runOrphanMQPostMerge error = %v, want unpreserved-branch refusal", err)
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted despite failed preservation: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
	if cleanup.RemoteDeleted || cleanup.LocalDeleted {
		t.Fatalf("cleanup claims a delete it did not do: %+v", cleanup)
	}
}

// TestRunOrphanMQPostMerge_RefusesMergeTargetBranch covers the guard the MR
// path gets from verifyMQPostMergeProof's source/target check: asked to clean
// "main" by name, the preservation proof would pass trivially (the target is
// preserved on itself) and the branch would be deleted.
func TestRunOrphanMQPostMerge_RefusesMergeTargetBranch(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"main", "master", "HEAD", "refs/heads/main"} {
		rigGit := testMQOrphanGit()

		_, err := runOrphanMQPostMerge(t.TempDir(), rigGit, branch, false, "")
		if err == nil || !strings.Contains(err.Error(), "merge target") {
			t.Fatalf("runOrphanMQPostMerge(%q) error = %v, want merge-target refusal", branch, err)
		}
		if len(rigGit.deletedBranches) != 0 {
			t.Fatalf("runOrphanMQPostMerge(%q) deleted %v", branch, rigGit.deletedBranches)
		}
		if len(rigGit.prunedRemotes) != 0 {
			t.Fatalf("runOrphanMQPostMerge(%q) hit the network before refusing", branch)
		}
	}
}

func TestRunOrphanMQPostMerge_RefusesMergeTargetBranchWhenDefaultIsNotMain(t *testing.T) {
	t.Parallel()
	rigGit := testMQOrphanGit()
	rigGit.defaultBranch = "trunk"

	_, err := runOrphanMQPostMerge(t.TempDir(), rigGit, "trunk", false, "")
	if err == nil || !strings.Contains(err.Error(), "merge target") {
		t.Fatalf("runOrphanMQPostMerge error = %v, want merge-target refusal", err)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted: %v", rigGit.deletedBranches)
	}
}

func TestRunOrphanMQPostMerge_OpenPRLeavesRemoteBranch(t *testing.T) {
	t.Parallel()
	rigGit := testMQOrphanGit()
	rigGit.openPR = true

	cleanup, err := runOrphanMQPostMerge(t.TempDir(), rigGit, testMQOrphanBranch, false, "")
	if err != nil {
		t.Fatalf("runOrphanMQPostMerge: %v", err)
	}
	if !cleanup.OpenPR {
		t.Fatalf("cleanup.OpenPR = false, cleanup=%+v", cleanup)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote branch deleted despite open PR: %v", rigGit.deletedBranches)
	}
	if !cleanup.LocalDeleted || len(rigGit.localDeleted) != 1 {
		t.Fatalf("local branch cleanup = %v, want [%s]", rigGit.localDeleted, testMQOrphanBranch)
	}
}

// TestRunOrphanMQPostMerge_MissingRemoteBranchIsNotSuccess keeps a typo'd MR id
// from reading as a completed cleanup: with no MR bead and no such branch,
// nothing was cleaned and the caller has to hear that.
func TestRunOrphanMQPostMerge_MissingRemoteBranchIsNotSuccess(t *testing.T) {
	t.Parallel()
	rigGit := testMQOrphanGit()
	rigGit.remoteTip = ""

	cleanup, err := runOrphanMQPostMerge(t.TempDir(), rigGit, testMQOrphanBranch, false, "")
	if err == nil || !strings.Contains(err.Error(), "no branch") || !strings.Contains(err.Error(), "nothing to clean") {
		t.Fatalf("runOrphanMQPostMerge error = %v, want a nothing-to-clean failure", err)
	}
	if cleanup.RemoteDeleted || cleanup.AlreadyGone {
		t.Fatalf("cleanup claims an outcome it did not reach: %+v", cleanup)
	}
	if len(rigGit.preservedAgainst) != 0 || len(rigGit.deletedBranches) != 0 {
		t.Fatalf("no-op acted anyway: preserved=%v deleted=%v", rigGit.preservedAgainst, rigGit.deletedBranches)
	}
}

// TestRunOrphanMQPostMerge_RefusesNonPolecatBranch keeps the orphan path inside
// the command's remit: a release or integration branch can satisfy the
// preservation proof while being a ref nobody asked post-merge to clean.
func TestRunOrphanMQPostMerge_RefusesNonPolecatBranch(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"release/1.2", "integration/epic-a", "develop", "adam/26/9/feature"} {
		rigGit := testMQOrphanGit()

		_, err := runOrphanMQPostMerge(t.TempDir(), rigGit, branch, false, "")
		if err == nil || !strings.Contains(err.Error(), "polecat/") {
			t.Fatalf("runOrphanMQPostMerge(%q) error = %v, want non-polecat refusal", branch, err)
		}
		if len(rigGit.prunedRemotes) != 0 || len(rigGit.deletedBranches) != 0 {
			t.Fatalf("runOrphanMQPostMerge(%q) acted before refusing: %v %v", branch, rigGit.prunedRemotes, rigGit.deletedBranches)
		}
	}
}

func TestRunOrphanMQPostMerge_PreservationErrorFailsClosed(t *testing.T) {
	t.Parallel()
	rigGit := testMQOrphanGit()
	rigGit.preserveErr = errors.New("candidate refs/heads/x changed while pruning")

	_, err := runOrphanMQPostMerge(t.TempDir(), rigGit, testMQOrphanBranch, false, "")
	if err == nil || !strings.Contains(err.Error(), "changed while pruning") {
		t.Fatalf("runOrphanMQPostMerge error = %v, want the comparison failure", err)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted after a failed comparison: %v", rigGit.deletedBranches)
	}
}

func TestRunOrphanMQPostMerge_FetchFailureFailsClosed(t *testing.T) {
	t.Parallel()
	rigGit := testMQOrphanGit()
	rigGit.pruneErr = errors.New("no network")

	_, err := runOrphanMQPostMerge(t.TempDir(), rigGit, testMQOrphanBranch, false, "")
	if err == nil || !strings.Contains(err.Error(), "no network") {
		t.Fatalf("runOrphanMQPostMerge error = %v, want the fetch failure", err)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted after a failed fetch: %v", rigGit.deletedBranches)
	}
}

func TestRunOrphanMQPostMerge_RefusesMRShapingFlags(t *testing.T) {
	t.Parallel()
	rigGit := testMQOrphanGit()

	_, err := runOrphanMQPostMerge(t.TempDir(), rigGit, testMQOrphanBranch, true, "")
	if err == nil || !strings.Contains(err.Error(), "--skip-branch-delete") {
		t.Fatalf("runOrphanMQPostMerge error = %v, want --skip-branch-delete refusal", err)
	}

	_, err = runOrphanMQPostMerge(t.TempDir(), rigGit, testMQOrphanBranch, false, "2bb0bf7f")
	if err == nil || !strings.Contains(err.Error(), "--landed-commit") {
		t.Fatalf("runOrphanMQPostMerge error = %v, want --landed-commit refusal", err)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted despite refused flags: %v", rigGit.deletedBranches)
	}
}

func TestRunOrphanMQPostMerge_NormalizesRefArg(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		testMQOrphanBranch,
		"refs/heads/" + testMQOrphanBranch,
		"origin/" + testMQOrphanBranch,
		"refs/remotes/origin/" + testMQOrphanBranch,
		"  " + testMQOrphanBranch + "  ",
	} {
		rigGit := testMQOrphanGit()

		cleanup, err := runOrphanMQPostMerge(t.TempDir(), rigGit, ref, false, "")
		if err != nil {
			t.Fatalf("runOrphanMQPostMerge(%q): %v", ref, err)
		}
		if cleanup.Branch != testMQOrphanBranch {
			t.Fatalf("runOrphanMQPostMerge(%q) branch = %q, want %q", ref, cleanup.Branch, testMQOrphanBranch)
		}
		if len(rigGit.preservedAgainst) != 1 || rigGit.preservedAgainst[0] != "refs/heads/"+testMQOrphanBranch {
			t.Fatalf("runOrphanMQPostMerge(%q) checked %v", ref, rigGit.preservedAgainst)
		}
	}
}

func TestResolveMQPostMerge_FallsBackToOrphanWhenMRBeadIsGone(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{findErr: refinery.ErrMRNotFound}
	rigGit := testMQOrphanGit()

	result, cleanup, orphan, err := resolveMQPostMerge(mgr, t.TempDir(), rigGit, testMQOrphanBranch, false, "")
	if err != nil {
		t.Fatalf("resolveMQPostMerge: %v", err)
	}
	if !orphan {
		t.Fatal("orphan = false, want the orphan path")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil for branch-only cleanup", result)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called with no MR bead")
	}
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the branch deleted", cleanup)
	}
}

// TestResolveMQPostMerge_KeepsRefusalsOnMRPath is the difference between "the MR
// is gone" and "the MR is here and this cleanup is refused": only the former
// may take the orphan path, or a refused MR would have its branch deleted by
// name instead of by proof.
func TestResolveMQPostMerge_KeepsRefusalsOnMRPath(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR(), postMergeErr: errors.New("MR status is not open or terminal")}
	rigGit := &fakeMQPostMergeGit{preserved: true, remoteTip: testMQOrphanTip, localHead: testMQOrphanTip}

	_, _, orphan, err := resolveMQPostMerge(mgr, t.TempDir(), rigGit, testMQPostMergeMR().Branch, false, "")
	if err == nil || !strings.Contains(err.Error(), "MR status is not open or terminal") {
		t.Fatalf("resolveMQPostMerge error = %v, want the MR refusal", err)
	}
	if orphan {
		t.Fatal("orphan = true for an MR that exists but refused cleanup")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted by the orphan path after an MR refusal: %v", rigGit.deletedBranches)
	}
}

// TestResolveMQPostMerge_ReportsBothFailures keeps the operator's next step
// visible when neither path applies.
func TestResolveMQPostMerge_ReportsBothFailures(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{findErr: refinery.ErrMRNotFound}
	rigGit := testMQOrphanGit()
	rigGit.preserved = false

	_, _, orphan, err := resolveMQPostMerge(mgr, t.TempDir(), rigGit, testMQOrphanBranch, false, "")
	if err == nil || !strings.Contains(err.Error(), "merge request not found") || !strings.Contains(err.Error(), "not preserved on origin/main") {
		t.Fatalf("resolveMQPostMerge error = %v, want both the missing MR and the unpreserved branch", err)
	}
	if orphan {
		t.Fatal("orphan = true after the orphan path failed")
	}
}

// initOrphanCleanupRepo builds a clone whose origin carries a polecat branch,
// optionally squash-merged into main, so the orphan path runs against a real
// remote rather than a fake. (gt-qjp2)
func initOrphanCleanupRepo(t *testing.T, squashMerge bool) (clone, branch string) {
	t.Helper()
	tmp := t.TempDir()
	originPath := filepath.Join(tmp, "origin.git")
	runOrphanCleanupGit(t, tmp, "init", "--bare", "-b", "main", originPath)

	clone = filepath.Join(tmp, "clone")
	runOrphanCleanupGit(t, tmp, "clone", originPath, clone)
	runOrphanCleanupGit(t, clone, "config", "user.email", "polecat@example.com")
	runOrphanCleanupGit(t, clone, "config", "user.name", "Polecat Test")

	writeOrphanCleanupFile(t, clone, "README.md", "seed\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "seed main")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", "main")

	branch = testMQOrphanBranch
	runOrphanCleanupGit(t, clone, "checkout", "-b", branch)
	writeOrphanCleanupFile(t, clone, "work.txt", "polecat work\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "polecat work")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", branch)

	if !squashMerge {
		return clone, branch
	}
	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--squash", branch)
	runOrphanCleanupGit(t, clone, "commit", "-m", "merge squash")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	return clone, branch
}

func runOrphanCleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeOrphanCleanupFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// orphanCleanupRealGit is a real *git.Git with the PR guard stubbed out. A
// temp-dir origin is not a GitHub repo, so the guard's lookup fails and
// HasOpenPullRequest reports it as protected (gas-fk4), which would hide the
// deletion this test is about. The guard's own behaviour is covered by the
// fake-git tests.
type orphanCleanupRealGit struct {
	*git.Git
}

func (orphanCleanupRealGit) HasOpenPullRequest(git.PullRequestRef) bool { return false }

// TestRunOrphanMQPostMerge_RealRemoteSquashMergedBranch pins the composition
// the fake cannot: the ref name the proof fetches (refs/heads/<branch>), the
// comparison ref (origin/main), and the lease delete at the live tip all have
// to agree with real git for a branch the refinery squash-merged to go away.
func TestRunOrphanMQPostMerge_RealRemoteSquashMergedBranch(t *testing.T) {
	t.Parallel()
	clone, branch := initOrphanCleanupRepo(t, true)

	cleanup, err := runOrphanMQPostMerge(t.TempDir(), orphanCleanupRealGit{git.NewGit(clone)}, branch, false, "")
	if err != nil {
		t.Fatalf("runOrphanMQPostMerge: %v", err)
	}
	if cleanup.Target != "origin/main" {
		t.Fatalf("cleanup.Target = %q, want origin/main", cleanup.Target)
	}
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the remote branch deleted", cleanup)
	}
	if remote := runOrphanCleanupGit(t, clone, "ls-remote", "--heads", "origin", branch); remote != "" {
		t.Fatalf("remote branch survived cleanup: %s", remote)
	}
}

// TestRunOrphanMQPostMerge_RealRemoteRefusesUnmergedBranch is the same
// composition on the refusing side: a branch whose work is not on main must
// still be there afterwards.
func TestRunOrphanMQPostMerge_RealRemoteRefusesUnmergedBranch(t *testing.T) {
	t.Parallel()
	clone, branch := initOrphanCleanupRepo(t, false)

	_, err := runOrphanMQPostMerge(t.TempDir(), orphanCleanupRealGit{git.NewGit(clone)}, branch, false, "")
	if err == nil || !strings.Contains(err.Error(), "not preserved on origin/main") {
		t.Fatalf("runOrphanMQPostMerge error = %v, want unpreserved-branch refusal", err)
	}
	if remote := runOrphanCleanupGit(t, clone, "ls-remote", "--heads", "origin", branch); remote == "" {
		t.Fatal("remote branch deleted despite unmerged work")
	}
}

// gitAllowFail runs git and returns its combined output without failing the
// test on a non-zero exit. Building a real conflict means asking git to do
// something it is supposed to refuse.
func gitAllowFail(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true", "GIT_MERGE_AUTOEDIT=no")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestVerifyMQPostMergeProof_RealConflictResolvedRebase is the git-level
// demonstration the review asked for (gt-mlla). Every other attestation test
// fakes the git calls, so nothing proved the premise the flag exists for
// actually holds in a real repository: that a conflict-resolved rebase leaves
// the submitted head genuinely unprovable on target while the commit the
// refinery pushed is provable, and that an unrelated on-target commit still is
// not.
//
// It builds the shape end to end — a branch and a concurrent target change
// editing the same line, a real conflict, a real resolution, then the queue's
// squash landing — and drives verifyMQPostMergeProof against it.
func TestVerifyMQPostMergeProof_RealConflictResolvedRebase(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	originPath := filepath.Join(tmp, "origin.git")
	runOrphanCleanupGit(t, tmp, "init", "--bare", "-b", "main", originPath)

	clone := filepath.Join(tmp, "clone")
	runOrphanCleanupGit(t, tmp, "clone", originPath, clone)
	runOrphanCleanupGit(t, clone, "config", "user.email", "polecat@example.com")
	runOrphanCleanupGit(t, clone, "config", "user.name", "Polecat Test")

	writeOrphanCleanupFile(t, clone, "app.txt", "line one\nline two\nline three\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "seed main")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", "main")

	const branch = "polecat/marlin/gt-mlla"
	runOrphanCleanupGit(t, clone, "checkout", "-b", branch)
	writeOrphanCleanupFile(t, clone, "app.txt", "line one\npolecat middle\nline three\n")
	runOrphanCleanupGit(t, clone, "commit", "-am", "polecat change")
	submittedHead := runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", branch)

	// The target moves underneath with a conflicting edit to the same line, so
	// replaying the branch is a real conflict rather than a clean rebase.
	runOrphanCleanupGit(t, clone, "checkout", "main")
	writeOrphanCleanupFile(t, clone, "app.txt", "line one\nmain middle\nline three\n")
	runOrphanCleanupGit(t, clone, "commit", "-am", "concurrent target change")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")

	// Resolve the way the conflict-resolution task instructs: replay onto the
	// moved target, settle the conflict, push the resolved head. That push is
	// what leaves the MR bead's commit_sha stale.
	runOrphanCleanupGit(t, clone, "checkout", branch)
	if out, err := gitAllowFail(t, clone, "rebase", "main"); err == nil {
		t.Fatalf("test premise broken: rebasing onto the moved target did not conflict:\n%s", out)
	}
	writeOrphanCleanupFile(t, clone, "app.txt", "line one\nresolved middle\nline three\n")
	runOrphanCleanupGit(t, clone, "add", "app.txt")
	if out, err := gitAllowFail(t, clone, "rebase", "--continue"); err != nil {
		t.Fatalf("rebase --continue: %v\n%s", err, out)
	}
	if runOrphanCleanupGit(t, clone, "rev-parse", "HEAD") == submittedHead {
		t.Fatal("test premise broken: conflict resolution left the submitted head unchanged")
	}
	runOrphanCleanupGit(t, clone, "push", "--force", "origin", branch)

	// Land it the way the sequential-rebase queue does: squash onto the moved
	// target, so the resolved head is never itself an ancestor of main.
	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--squash", branch)
	runOrphanCleanupGit(t, clone, "commit", "-m", "queue: land "+branch)
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	landedCommit := runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")

	g := git.NewGit(clone)
	mr := &refinery.MergeRequest{
		ID:           "gt-mr-real-attestation",
		Branch:       branch,
		TargetBranch: "main",
		CommitSHA:    submittedHead,
	}

	// The premise: the stale submitted head is neither on target nor
	// content-preserved there, so the default proof cannot hold for it.
	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", "main", submittedHead); err == nil {
		t.Fatal("test premise broken: the stale submitted head satisfied the default proof")
	}
	if _, err := verifyMQPostMergeProof(g, mr, ""); err == nil {
		t.Fatal("verifyMQPostMergeProof accepted the stale submitted head with no attestation")
	}

	// The attested landed commit is the one the refinery actually pushed.
	recorded, err := verifyMQPostMergeProof(g, mr, landedCommit)
	if err != nil {
		t.Fatalf("verifyMQPostMergeProof with the real landed commit: %v", err)
	}
	if recorded != landedCommit {
		t.Fatalf("recorded merge commit = %q, want the landed squash commit %q", recorded, landedCommit)
	}

	// An abbreviated attestation must persist as the full SHA.
	recorded, err = verifyMQPostMergeProof(g, mr, landedCommit[:7])
	if err != nil {
		t.Fatalf("verifyMQPostMergeProof with an abbreviated attestation: %v", err)
	}
	if recorded != landedCommit {
		t.Fatalf("recorded merge commit = %q, want the abbreviated attestation expanded to %q", recorded, landedCommit)
	}

	// Negative control: a commit that IS reachable from target — because it is
	// target's tip — but landed unrelated work must still not attest for this
	// MR, or the flag would accept anything the repository already contains.
	writeOrphanCleanupFile(t, clone, "notes.md", "unrelated\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "unrelated target change")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	unrelatedOnTarget := runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")

	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", "main", unrelatedOnTarget); err != nil {
		t.Fatalf("test premise broken: the unrelated commit is not on target: %v", err)
	}
	if _, err := verifyMQPostMergeProof(g, mr, unrelatedOnTarget); err == nil {
		t.Fatal("verifyMQPostMergeProof accepted an unrelated on-target commit as attestation")
	}
}
