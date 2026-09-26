package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	verifyErr   error
	openPR      bool
	prUnknown   bool
	prLookupErr error
	deleteErr   error
	remoteTip   string
	localHead   string
	tipErr      error

	// Attestation-binding fixtures (verifyLandedCommitMatchesSubmitted).
	mergeBase       string
	mergeBaseErr    error
	landedParent    string
	landedParentErr error
	submittedFiles  []string
	landedFiles     []string
	diffErr         error

	// Patch-id fixtures (submittedRangeLandedByPatchID): the MR's own commits,
	// and the patch-id of each commit met walking back from the attested one.
	// The last landed id repeats, standing in for the target history the walk
	// stops at.
	submittedPatchIDs []string
	landedOwnPatchIDs []string
	patchIDSteps      int
	patchIDsErr       error

	// resolvedCommit is what Rev returns for an attested commit, modelling git's
	// abbreviated-SHA expansion (resolveMQPostMergeCommit).
	resolvedCommit string
	resolveErr     error

	// targetPriorTip is what Rev returns for the target ref's parent (e.g.
	// "origin/main^"), the inferred-head vacuous-proof guard's read of
	// target's state before its most recent commit (gt-6o1u).
	targetPriorTip    string
	targetPriorTipErr error

	// liveTip is what PushRemoteBranchTip returns: the branch's live remote
	// tip. Unset, it falls back to remoteTip (a static tip).
	liveTip string

	// tipSequence, when set, returns one value per successive
	// PushRemoteBranchTip call instead of a single static one, so a test can
	// model the remote tip moving between an inferred-head proof's own read
	// and the branch-delete cleanup's later read of the same branch
	// (gt-6o1u). The last entry repeats for any call past the end.
	tipSequence []string
	tipSeqCalls int

	// Orphan-branch cleanup fixtures (runOrphanMQPostMerge).
	defaultBranch string
	targetRef     string
	pruneErr      error
	preserved     bool
	unpreserved   int
	preserveErr   error

	verifiedCommits  []string
	branchTipReads   int
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

func (g *fakeMQPostMergeGit) PullRequestProtection(git.PullRequestRef) (git.PRProtection, error) {
	if g.prUnknown {
		return git.PRProtectionUnknown, g.prLookupErr
	}
	if g.openPR {
		return git.PRProtectionOpen, nil
	}
	return git.PRProtectionNone, nil
}

func (g *fakeMQPostMergeGit) PushRemoteBranchTip(_, _ string) (string, error) {
	g.branchTipReads++
	if len(g.tipSequence) > 0 {
		idx := g.tipSeqCalls
		if idx >= len(g.tipSequence) {
			idx = len(g.tipSequence) - 1
		}
		g.tipSeqCalls++
		return g.tipSequence[idx], g.tipErr
	}
	if g.liveTip != "" {
		return g.liveTip, g.tipErr
	}
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
	if base, ok := strings.CutSuffix(ref, "^"); ok {
		// A qualified ref (e.g. "origin/main^") resolves target's state before
		// its most recent commit, for the inferred-head vacuous-proof guard
		// (gt-6o1u); a bare SHA is an attested commit's parent
		// (verifyLandedCommitMatchesSubmitted). Real refs always carry a "/",
		// a landed-commit SHA never does.
		if strings.Contains(base, "/") {
			return g.targetPriorTip, g.targetPriorTipErr
		}
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

// PatchIDs returns the submitted range's fixture. An unset fixture is an empty
// range, which reads as "no patch-id evidence" and leaves the file-level
// binding to decide, as before this probe existed.
func (g *fakeMQPostMergeGit) PatchIDs(_, _ string) ([]string, error) {
	if g.patchIDsErr != nil {
		return nil, g.patchIDsErr
	}
	return g.submittedPatchIDs, nil
}

// PatchID answers one step of the walk back from the attested commit, in order,
// repeating the last fixture so the walk stops on its own terms.
func (g *fakeMQPostMergeGit) PatchID(_, _ string) (string, error) {
	if g.patchIDsErr != nil {
		return "", g.patchIDsErr
	}
	if len(g.landedOwnPatchIDs) == 0 {
		return "", nil
	}
	step := g.patchIDSteps
	g.patchIDSteps++
	if step >= len(g.landedOwnPatchIDs) {
		step = len(g.landedOwnPatchIDs) - 1
	}
	return g.landedOwnPatchIDs[step], nil
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

// TestRunVerifiedMQPostMerge_MultiCommitAttestationLandedAsItsOwnCommits covers
// the reported incident (gt-fq4e): the queue landed the MR's commits
// individually — a fast-forward of a rebased branch, as gt done's direct-merge
// convoy pushes — so the attested commit carries only the last commit's change
// and the file-level binding alone rejected a merge whose work had all landed.
// Every commit of the submitted range being on target by patch-id has to
// satisfy the proof.
func TestRunVerifiedMQPostMerge_MultiCommitAttestationLandedAsItsOwnCommits(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const landedCommit = "a1baa862cdfb5f2ea382ac2a67e8527dee0d5daa"
	rigGit := &fakeMQPostMergeGit{
		mergeBase:    "oldbase000",
		landedParent: "oldmain111",
		// The MR touched both files; the attested commit is the branch's last
		// one, which touched only the test file. Walking back from it reaches
		// the go-file commit, then the target history the MR was rebased onto.
		submittedFiles:    []string{"internal/daemon/jsonl_git_backup.go", "internal/daemon/jsonl_git_backup_test.go"},
		landedFiles:       []string{"internal/daemon/jsonl_git_backup_test.go"},
		submittedPatchIDs: []string{"patchid-go", "patchid-test"},
		landedOwnPatchIDs: []string{"patchid-test", "patchid-go", "patchid-target"},
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, landedCommit)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called: a multi-commit MR landed as its own commits was refused")
	}
	if mgr.postMergeMR.MergeCommit != landedCommit {
		t.Fatalf("MR MergeCommit = %q, want attested landed commit %q", mgr.postMergeMR.MergeCommit, landedCommit)
	}
}

// TestRunVerifiedMQPostMerge_SingleCommitAttestationLandedAsItsOwnCommit is the
// one-commit shape of the same landing: nothing in the attested commit's diff
// names the submitted file, so only the patch-id binding can admit it.
func TestRunVerifiedMQPostMerge_SingleCommitAttestationLandedAsItsOwnCommit(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const landedCommit = "99523cfa0e82ab9e44f915a1939bea8010578070"
	rigGit := &fakeMQPostMergeGit{
		mergeBase:         "oldbase000",
		landedParent:      "oldmain111",
		submittedFiles:    []string{"internal/cmd/mq.go"},
		landedFiles:       []string{"docs/unrelated.md"},
		submittedPatchIDs: []string{"patchid-mqgo"},
		landedOwnPatchIDs: []string{"patchid-mqgo", "patchid-target"},
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, landedCommit)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called for a single-commit MR present on target by patch-id")
	}
}

// TestRunVerifiedMQPostMerge_MultiCommitAttestationRefusedWhenACommitIsMissing
// is the fail-closed half (gt-fq4e): an MR whose later commits never landed
// carries patch-ids target does not have, and files the attested commit never
// touched. Neither binding may accept it.
func TestRunVerifiedMQPostMerge_MultiCommitAttestationRefusedWhenACommitIsMissing(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const landedCommit = "9af8b303518ec9dde1d4e6e4cf2444fb166bcf5a"
	rigGit := &fakeMQPostMergeGit{
		mergeBase:         "oldbase000",
		landedParent:      "oldmain111",
		submittedFiles:    []string{"impl.go", "impl_test.go"},
		landedFiles:       []string{"impl.go"},
		submittedPatchIDs: []string{"patchid-impl-test", "patchid-impl"},
		// The walk reaches one of the two commits, then target history.
		landedOwnPatchIDs: []string{"patchid-impl", "patchid-other"},
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, landedCommit)
	if err == nil || !strings.Contains(err.Error(), "impl_test.go") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want refusal naming the commit that did not land", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called for a partially-landed MR")
	}
}

// TestRunVerifiedMQPostMerge_EmptyPatchIDRangeIsNotProof pins the guard on the
// patch-id binding: an empty submitted range reads as "nothing to preserve",
// which must never be taken as evidence that the MR landed.
func TestRunVerifiedMQPostMerge_EmptyPatchIDRangeIsNotProof(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	const landedCommit = "a1baa862cdfb5f2ea382ac2a67e8527dee0d5daa"
	rigGit := &fakeMQPostMergeGit{
		mergeBase:         "oldbase000",
		landedParent:      "oldmain111",
		submittedFiles:    []string{"impl.go", "impl_test.go"},
		landedFiles:       []string{"impl_test.go"},
		landedOwnPatchIDs: []string{"patchid-impl", "patchid-impl-test"},
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, landedCommit)
	if err == nil || !strings.Contains(err.Error(), "impl.go") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want file-level refusal", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called with no patch-id evidence and an uncovered file")
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

// TestRunVerifiedMQPostMerge_InferredHeadMovedTipRefusedWhenNotPreserved is
// the regression for the branch-delete safety check being skipped on the
// inferred-head path (gt-6o1u): resolveMovedMQPostMergeBranchTip used to
// return an unmerged branch's live tip unchecked whenever the snapshot's head
// was inferred, so a push after the proof but before cleanup got force-
// deleted out from under the branch with no preservation check at all. The
// branch's own tip moves between inference's read (the head the proof
// verifies) and the cleanup step's later read of the same branch — modelled
// here with tipSequence — and that later tip is not preserved on target, so
// the delete must be refused exactly as it already is for a recorded head
// (TestRunVerifiedMQPostMerge_MovedBranchTipRefusedWhenNotPreserved).
func TestRunVerifiedMQPostMerge_InferredHeadMovedTipRefusedWhenNotPreserved(t *testing.T) {
	t.Parallel()
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	const inferredHead = "abc123def456abc123def456abc123def456abcd"
	const movedTip = "1089ca701089ca701089ca701089ca701089ca70"
	rigGit := &fakeMQPostMergeGit{
		tipSequence: []string{inferredHead, movedTip},
		preserved:   false,
		unpreserved: 3,
	}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "not preserved on origin/main") || !strings.Contains(err.Error(), "3 patch-unique commits") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want unpreserved-moved-tip refusal", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted despite an unpreserved moved tip on an inferred head: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
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

// TestRunVerifiedMQPostMerge_PRLookupFailureIsReportedAsUnknownNotOpenPR is the
// regression for gt-ghpk: a PR lookup that fails (no GitHub remote, gh
// missing/unauthenticated, an ambiguous head match) must not be reported as
// "open PR exists" — that sends the operator hunting for a PR that was never
// found. It still has to fail closed and leave the remote branch alone, same
// as a genuinely open PR, but the caller needs to be able to tell the two
// apart.
func TestRunVerifiedMQPostMerge_PRLookupFailureIsReportedAsUnknownNotOpenPR(t *testing.T) {
	t.Parallel()
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	lookupErr := errors.New("remote is not a GitHub repo")
	rigGit := &fakeMQPostMergeGit{prUnknown: true, prLookupErr: lookupErr, localHead: mgr.mr.CommitSHA}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if !cleanup.PRUnknown {
		t.Fatalf("cleanup.PRUnknown = false, cleanup=%+v", cleanup)
	}
	if cleanup.OpenPR {
		t.Fatalf("cleanup.OpenPR = true, want false for a failed lookup: cleanup=%+v", cleanup)
	}
	if !errors.Is(cleanup.PRLookupErr, lookupErr) {
		t.Fatalf("cleanup.PRLookupErr = %v, want %v", cleanup.PRLookupErr, lookupErr)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote branch deleted despite unresolved PR lookup: %v", rigGit.deletedBranches)
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
	// The branch the MR records has no remote tip either, so there is nothing
	// left to resolve the submitted head from (gt-6o1u): refusing is the only
	// honest answer, and the branch must survive it.
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

// TestRunVerifiedMQPostMerge_InfersSubmittedHeadFromBranch is the recovery the
// bead asks for: a directly-created MR (no commit_sha) whose recorded branch
// still exists is completed rather than refused, with the inferred head proven
// against the target and used for the branch-delete lease.
func TestRunVerifiedMQPostMerge_InfersSubmittedHeadFromBranch(t *testing.T) {
	t.Parallel()
	const tip = "9c1327d59c1327d59c1327d59c1327d59c1327d5"
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{remoteTip: tip, localHead: tip}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called for a directly-created MR")
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != tip {
		t.Fatalf("verified commits = %v, want the inferred branch tip %s", rigGit.verifiedCommits, tip)
	}
	// The snapshot handed on keeps the bead's own (empty) commit_sha so the
	// close-time CAS compares against what the bead actually recorded, and
	// carries the recovered head in the fields that distinguish evidence
	// from submission (gt-6o1u). The branch delete's lease is pinned to the
	// same head, in cleanup.SubmittedHead below.
	if mgr.postMergeMR.CommitSHA != "" {
		t.Fatalf("MR snapshot commit_sha = %q, want empty (the bead records no commit_sha; inference must not rewrite the snapshot)", mgr.postMergeMR.CommitSHA)
	}
	if !mgr.postMergeMR.CommitSHAInferred {
		t.Fatalf("MR snapshot CommitSHAInferred = false, want true for a head read off the branch")
	}
	if mgr.postMergeMR.VerifiedHead != tip {
		t.Fatalf("MR snapshot VerifiedHead = %q, want the inferred tip %s", mgr.postMergeMR.VerifiedHead, tip)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != tip {
		t.Fatalf("delete lease heads = %v, want [%s]", rigGit.deletedHeads, tip)
	}
	if cleanup.SubmittedHead != tip || !cleanup.SubmittedHeadInferred {
		t.Fatalf("cleanup = %+v, want the inferred head %s reported as inferred", cleanup, tip)
	}
}

// TestRunVerifiedMQPostMerge_InferredHeadAlreadyOnTargetRefuses is the
// vacuous-proof guard: an inferred head that is already an ancestor of the
// target carries no commits of the branch's own, so reachability would pass
// for the wrong reason — the branch was replaced, nothing of its own work
// landed. Refuse with the re-record recovery named.
func TestRunVerifiedMQPostMerge_InferredHeadAlreadyOnTargetRefuses(t *testing.T) {
	t.Parallel()
	const tip = "9c1327d59c1327d59c1327d59c1327d59c1327d5"
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	// targetPriorTip gives target a parent to compare against (a real repo
	// has one); mergeBase == the inferred tip says the tip is already an
	// ancestor of it (an empty range: base..tip carries no commits of its
	// own).
	rigGit := &fakeMQPostMergeGit{remoteTip: tip, localHead: tip, targetPriorTip: "oldmain000oldmain000oldmain000oldmain000", mergeBase: tip}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "already an ancestor of") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want vacuous-range refusal", err)
	}
	if !strings.Contains(err.Error(), "re-record commit_sha") {
		t.Fatalf("proof error %q does not name the recovery (re-record commit_sha)", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called for an inferred head that is already on the target")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted for a vacuous proof: %v", rigGit.deletedBranches)
	}
	// The merge-base check is inference-specific and runs before the
	// reachability proof: the tip must never have reached the verifier.
	if len(rigGit.verifiedCommits) != 0 {
		t.Fatalf("verified commits = %v, want none (refused at the merge-base check)", rigGit.verifiedCommits)
	}
}

// TestRunVerifiedMQPostMerge_InferredHeadAlreadyOnTargetRefusesEvenIfVerified
// proves the guard cannot be waved through with a permissive reachability
// fake: the same refusal holds when the verifier reports success.
func TestRunVerifiedMQPostMerge_InferredHeadAlreadyOnTargetRefusesEvenIfVerified(t *testing.T) {
	t.Parallel()
	const tip = "9c1327d59c1327d59c1327d59c1327d59c1327d5"
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{remoteTip: tip, localHead: tip, targetPriorTip: "oldmain000oldmain000oldmain000oldmain000", mergeBase: tip}
	// The fake verifier has no verifyErr, i.e. it "succeeds": the refusal must
	// come from the merge-base check alone, not from reachability.
	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "already an ancestor of") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want vacuous-range refusal even with a permissive verifier", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called despite a permissive verifier for an ancestor head")
	}
}

// TestRunVerifiedMQPostMerge_MissingTargetRefusesBeforeInference pins the
// ordering the rework requires: the verifier's structural checks run before
// any inference touches the branch. A directly-created MR that records no
// target is refused without a single branch-tip read.
func TestRunVerifiedMQPostMerge_MissingTargetRefusesBeforeInference(t *testing.T) {
	t.Parallel()
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mr.TargetBranch = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	// A live tip exists, so a premature inference would succeed and mask the
	// structural refusal.
	rigGit := &fakeMQPostMergeGit{remoteTip: "9c1327d59c1327d59c1327d59c1327d59c1327d5"}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "missing target branch") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want missing target refusal", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called for an MR with no target branch")
	}
	if rigGit.branchTipReads != 0 {
		t.Fatalf("branch tip reads = %d, want 0 (target check refuses before inference)", rigGit.branchTipReads)
	}
}

// TestRunVerifiedMQPostMerge_RecordedInferredHeadIsNotReInferred covers
// post-close idempotency: once the close path has recorded the recovered head
// on the bead (commit_sha plus commit_sha_inferred), the bead is the record
// and the head is trusted as submitted — re-reading the branch tip would
// re-verify against a tip the branch may have moved since.
func TestRunVerifiedMQPostMerge_RecordedInferredHeadIsNotReInferred(t *testing.T) {
	t.Parallel()
	const recorded = "abc123def456"
	const movedTip = "beef1234beef1234beef1234beef1234beef1234"
	mr := testMQPostMergeMR()
	mr.CommitSHA = recorded
	mr.CommitSHAInferred = true
	mgr := &fakeMQPostMergeManager{mr: mr}
	// liveTip pins the tip the lease's own read sees; remoteTip (the moved
	// tip) is what inference would have read, which must not happen.
	rigGit := &fakeMQPostMergeGit{remoteTip: movedTip, localHead: movedTip, liveTip: recorded, preserved: true}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != recorded {
		t.Fatalf("verified commits = %v, want the recorded head %q (the moved tip must not replace it)", rigGit.verifiedCommits, recorded)
	}
	if cleanup.SubmittedHeadInferred {
		t.Fatalf("cleanup = %+v, want no inference for a head the bead already records", cleanup)
	}
	// No inference, so exactly the one branch-tip read the lease itself makes.
	if rigGit.branchTipReads != 1 {
		t.Fatalf("branch tip reads = %d, want 1 (no re-inference of a recorded head; only the lease's own read)", rigGit.branchTipReads)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != recorded {
		t.Fatalf("deleted heads = %v, want the recorded head %q", rigGit.deletedHeads, recorded)
	}
}

// TestRunVerifiedMQPostMerge_InferredHeadNotOnTargetFailsClosed is the other
// half of the contract: inference chooses which commit is proven, never whether
// it has to be on the target. An MR with no commit_sha whose branch never
// landed must not close, delete its branch, or record anything.
func TestRunVerifiedMQPostMerge_InferredHeadNotOnTargetFailsClosed(t *testing.T) {
	t.Parallel()
	const tip = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{remoteTip: tip, verifyErr: errors.New("not reachable")}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "merge proof failed") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want merge proof failure", err)
	}
	if !strings.Contains(err.Error(), tip) {
		t.Fatalf("proof error %q does not name the head it refused (%s)", err, tip)
	}
	// The bead's third request: the refusal has to read as "this head is not on
	// the target", not as the rebase fight it is easy to mistake it for.
	if !strings.Contains(err.Error(), "inferred from branch") {
		t.Fatalf("proof error %q does not say the head was inferred", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called after failed proof")
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted after failed proof: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}

// TestRunVerifiedMQPostMerge_InferUnavailableNamesTheRecovery covers the one
// remaining dead end: no commit_sha and no branch to read one from. That is a
// metadata problem, so the refusal names the metadata recovery rather than
// leaving an operator to assume the merge or the rebase is at fault.
func TestRunVerifiedMQPostMerge_InferUnavailableNamesTheRecovery(t *testing.T) {
	t.Parallel()
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mr.Branch = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "missing submitted commit_sha") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want missing submitted head", err)
	}
	if !strings.Contains(err.Error(), "re-record commit_sha") {
		t.Fatalf("proof error %q does not name the recovery (re-record commit_sha)", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called without a provable head")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted without a provable head: %v", rigGit.deletedBranches)
	}
}

// TestRunVerifiedMQPostMerge_RecordedHeadIsNotInferred pins the direction of
// the inference. A recorded commit_sha is a claim about what was submitted and
// outranks the branch's current tip, which a conflict-resolution push may
// legitimately have moved past it (gt-lk6g). Inference fills an absent field;
// it never overrides a present one.
func TestRunVerifiedMQPostMerge_RecordedHeadIsNotInferred(t *testing.T) {
	t.Parallel()
	const movedTip = "beef1234beef1234beef1234beef1234beef1234"
	const targetTip = "aaaa5678aaaa5678aaaa5678aaaa5678aaaa5678"
	mr := testMQPostMergeMR()
	mgr := &fakeMQPostMergeManager{mr: mr}
	// The branch tip has moved since submission, so the live head (remote or
	// local) differs from the recorded head. The move is content-preserved
	// (preserved: true), which is the one advance the move-past resolution is
	// designed to absorb (gt-lk6g) — the lease pin moves with the preserved
	// tip, it does not refuse.
	rigGit := &fakeMQPostMergeGit{remoteTip: movedTip, localHead: movedTip, liveTip: targetTip, preserved: true}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != mr.CommitSHA {
		t.Fatalf("verified commits = %v, want the recorded %q", rigGit.verifiedCommits, mr.CommitSHA)
	}
	if mgr.postMergeMR.CommitSHA != mr.CommitSHA {
		t.Fatalf("snapshot commit_sha = %q, want the recorded %q (the branch tip must not replace it)",
			mgr.postMergeMR.CommitSHA, mr.CommitSHA)
	}
	if cleanup.SubmittedHead != mr.CommitSHA {
		t.Fatalf("cleanup = %+v, want the recorded head for an MR that records its head", cleanup)
	}
	// A recorded head is verified as-is: no inference, so exactly the one
	// branch-tip read the lease itself makes. The move-past resolution sees
	// the preserved move and pins the lease to the live tip — the recorded
	// head stays the snapshot's commit_sha and the submission identity.
	if rigGit.branchTipReads != 1 {
		t.Fatalf("branch tip reads = %d, want 1 (only the lease's own read)", rigGit.branchTipReads)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != targetTip {
		t.Fatalf("deleted heads = %v, want the preserved live tip %q (the recorded pin moves with a preserved advance)",
			rigGit.deletedHeads, targetTip)
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
	// The structural refusal must happen before inference: no branch-tip
	// read at all, even though this MR also records no commit_sha.
	if rigGit.branchTipReads != 0 {
		t.Fatalf("branch tip reads = %d, want 0 (source/target refused before inference)", rigGit.branchTipReads)
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
// temp-dir origin is not a GitHub repo, so the guard's lookup fails and would
// otherwise report Unknown (gt-ghpk) — still protected (gas-fk4) — which
// would hide the deletion this test is about. The guard's own behaviour is
// covered by the fake-git tests.
type orphanCleanupRealGit struct {
	*git.Git
}

func (orphanCleanupRealGit) PullRequestProtection(git.PullRequestRef) (git.PRProtection, error) {
	return git.PRProtectionNone, nil
}

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
	if _, err := verifyMQPostMergeProof(g, mr, mr.CommitSHA, ""); err == nil {
		t.Fatal("verifyMQPostMergeProof accepted the stale submitted head with no attestation")
	}

	// The attested landed commit is the one the refinery actually pushed.
	recorded, err := verifyMQPostMergeProof(g, mr, mr.CommitSHA, landedCommit)
	if err != nil {
		t.Fatalf("verifyMQPostMergeProof with the real landed commit: %v", err)
	}
	if recorded != landedCommit {
		t.Fatalf("recorded merge commit = %q, want the landed squash commit %q", recorded, landedCommit)
	}

	// An abbreviated attestation must persist as the full SHA.
	recorded, err = verifyMQPostMergeProof(g, mr, mr.CommitSHA, landedCommit[:7])
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
	if _, err := verifyMQPostMergeProof(g, mr, mr.CommitSHA, unrelatedOnTarget); err == nil {
		t.Fatal("verifyMQPostMergeProof accepted an unrelated on-target commit as attestation")
	}
}

// mrCleanupRepo is a bare origin carrying the shape the refinery reported on
// 2026-09-16 (gt-mkut), built once per test that needs it.
type mrCleanupRepo struct {
	originPath     string
	branch         string
	submittedHead  string
	resolutionHead string
	landedCommit   string
}

// initConflictResolvedMRRepo builds the incident end to end in a real
// repository: a branch submitted at submittedHead, a concurrent target change
// to the same line, the conflict-resolution merge pushed to the branch after
// submission (which is what leaves the MR bead's commit_sha stale), and the
// refinery's no-ff merge of that branch into main. The returned origin is bare
// and untouched by any assertion, so each test clones it and observes the
// remote delete independently.
func initConflictResolvedMRRepo(t *testing.T) mrCleanupRepo {
	t.Helper()
	repo := mrCleanupRepo{branch: "polecat/amethyst/gt-mkut"}

	tmp := t.TempDir()
	repo.originPath = filepath.Join(tmp, "origin.git")
	runOrphanCleanupGit(t, tmp, "init", "--bare", "-b", "main", repo.originPath)

	clone := filepath.Join(tmp, "clone")
	runOrphanCleanupGit(t, tmp, "clone", repo.originPath, clone)
	runOrphanCleanupGit(t, clone, "config", "user.email", "polecat@example.com")
	runOrphanCleanupGit(t, clone, "config", "user.name", "Polecat Test")

	writeOrphanCleanupFile(t, clone, "app.txt", "line one\nline two\nline three\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "seed main")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", "main")

	runOrphanCleanupGit(t, clone, "checkout", "-b", repo.branch)
	writeOrphanCleanupFile(t, clone, "app.txt", "line one\npolecat middle\nline three\n")
	runOrphanCleanupGit(t, clone, "commit", "-am", "polecat change")
	repo.submittedHead = runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", repo.branch)

	// The target moves underneath with a conflicting edit to the same line, so
	// replaying the branch is a real conflict rather than a clean rebase.
	runOrphanCleanupGit(t, clone, "checkout", "main")
	writeOrphanCleanupFile(t, clone, "app.txt", "line one\nmain middle\nline three\n")
	runOrphanCleanupGit(t, clone, "commit", "-am", "concurrent target change")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	movedTarget := runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")

	runOrphanCleanupGit(t, clone, "checkout", repo.branch)
	if out, err := gitAllowFail(t, clone, "merge", movedTarget); err == nil {
		t.Fatalf("test premise broken: merging the moved target did not conflict:\n%s", out)
	}
	writeOrphanCleanupFile(t, clone, "app.txt", "line one\nresolved middle\nline three\n")
	runOrphanCleanupGit(t, clone, "add", "app.txt")
	runOrphanCleanupGit(t, clone, "commit", "-m", "resolve conflict")
	repo.resolutionHead = runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	if repo.resolutionHead == repo.submittedHead {
		t.Fatal("test premise broken: conflict resolution left the submitted head unchanged")
	}
	runOrphanCleanupGit(t, clone, "push", "origin", repo.branch)

	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--no-ff", "-m", "merge "+repo.branch, "origin/"+repo.branch)
	repo.landedCommit = runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	return repo
}

func cloneMRCleanupRepo(t *testing.T, originPath string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	runOrphanCleanupGit(t, filepath.Dir(clone), "clone", originPath, clone)
	return clone
}

func mrCleanupRequest(repo mrCleanupRepo) *refinery.MergeRequest {
	return &refinery.MergeRequest{
		ID:           "gt-mr-conflict-resolved",
		Branch:       repo.branch,
		Worker:       "polecats/amethyst",
		IssueID:      "gt-mkut",
		TargetBranch: "main",
		CommitSHA:    repo.submittedHead,
	}
}

// TestRunVerifiedMQPostMerge_RealRemoteConflictResolvedBranch is the failing
// path the bead reports, composed against real git: the branch tip has moved
// past the MR bead's stale commit_sha (the conflict-resolution push), so the
// lease delete pinned to that stale value is one the remote rejects, and the
// live tip is not the submitted head either. It must still go away, and only
// because the live tip's work is provably on main.
func TestRunVerifiedMQPostMerge_RealRemoteConflictResolvedBranch(t *testing.T) {
	t.Parallel()
	repo := initConflictResolvedMRRepo(t)
	clone := cloneMRCleanupRepo(t, repo.originPath)
	g := orphanCleanupRealGit{git.NewGit(clone)}

	mgr := &fakeMQPostMergeManager{mr: mrCleanupRequest(repo)}
	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), g, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the remote branch deleted at the live tip", cleanup)
	}
	if remote := runOrphanCleanupGit(t, clone, "ls-remote", "--heads", "origin", repo.branch); remote != "" {
		t.Fatalf("remote branch survived cleanup: %s", remote)
	}
	if cleanup.Target != "" {
		t.Fatalf("cleanup.Target = %q, want empty on the MR path", cleanup.Target)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called on the conflict-resolved path")
	}
}

// TestRunVerifiedMQPostMerge_RealRemoteConflictResolvedBranchWithAttestation
// is the same repository driven the other way, which is the second half of the
// bead: the operator attests the merge commit the refinery pushed. A fresh
// clone stands in for the retry, so the branch is still there and the attested
// commit is the one the live tip yields to.
func TestRunVerifiedMQPostMerge_RealRemoteConflictResolvedBranchWithAttestation(t *testing.T) {
	t.Parallel()
	repo := initConflictResolvedMRRepo(t)
	clone := cloneMRCleanupRepo(t, repo.originPath)
	g := orphanCleanupRealGit{git.NewGit(clone)}

	mgr := &fakeMQPostMergeManager{mr: mrCleanupRequest(repo)}
	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), g, mgr.mr.ID, false, repo.landedCommit)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge with --landed-commit: %v", err)
	}
	// The resolution merge makes the live tip itself an ancestor of main, so
	// the binding records that tip rather than the attested refiner commit: the
	// contract is "the commit recorded demonstrably landed", not "the recorded
	// commit is the one the operator typed" (see
	// verifyLandedCommitMatchesSubmitted).
	if mgr.postMergeMR == nil || mgr.postMergeMR.MergeCommit == "" {
		t.Fatalf("recorded merge commit = %+v, want the commit that landed", mgr.postMergeMR)
	}
	if repo.resolutionHead != mgr.postMergeMR.MergeCommit {
		t.Fatalf("recorded merge commit = %q, want the live tip %q that landed on main",
			mgr.postMergeMR.MergeCommit, repo.resolutionHead)
	}
	runOrphanCleanupGit(t, clone, "merge-base", "--is-ancestor", mgr.postMergeMR.MergeCommit, "origin/main")
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the remote branch deleted", cleanup)
	}
	if remote := runOrphanCleanupGit(t, clone, "ls-remote", "--heads", "origin", repo.branch); remote != "" {
		t.Fatalf("remote branch survived cleanup: %s", remote)
	}
}

// TestRunVerifiedMQPostMerge_RealRemoteRefusesUnmergedMovedTip is the guard
// side of the same composition: the submitted head landed, but the branch then
// picked up work that is not on main at all. Adopting the live tip there would
// delete unmerged commits, so the delete must be refused and the branch must
// survive.
func TestRunVerifiedMQPostMerge_RealRemoteRefusesUnmergedMovedTip(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	originPath := filepath.Join(tmp, "origin.git")
	runOrphanCleanupGit(t, tmp, "init", "--bare", "-b", "main", originPath)

	clone := filepath.Join(tmp, "clone")
	runOrphanCleanupGit(t, tmp, "clone", originPath, clone)
	runOrphanCleanupGit(t, clone, "config", "user.email", "polecat@example.com")
	runOrphanCleanupGit(t, clone, "config", "user.name", "Polecat Test")

	writeOrphanCleanupFile(t, clone, "README.md", "seed\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "seed main")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", "main")

	const branch = "polecat/amethyst/gt-mkut-unmerged"
	runOrphanCleanupGit(t, clone, "checkout", "-b", branch)
	writeOrphanCleanupFile(t, clone, "work.txt", "submitted work\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "submitted work")
	submittedHead := runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", branch)

	// The submitted work lands, satisfying the MR's merge proof...
	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--squash", branch)
	runOrphanCleanupGit(t, clone, "commit", "-m", "land submitted work")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")

	// ...but the branch keeps moving with work main never saw.
	runOrphanCleanupGit(t, clone, "checkout", branch)
	writeOrphanCleanupFile(t, clone, "work.txt", "submitted work\nunmerged follow-up\n")
	runOrphanCleanupGit(t, clone, "commit", "-am", "unmerged follow-up")
	runOrphanCleanupGit(t, clone, "push", "origin", branch)

	_, cleanup, err := runVerifiedMQPostMerge(
		&fakeMQPostMergeManager{mr: &refinery.MergeRequest{
			ID:           "gt-mr-unmerged-tip",
			Branch:       branch,
			TargetBranch: "main",
			CommitSHA:    submittedHead,
		}},
		t.TempDir(), orphanCleanupRealGit{git.NewGit(clone)}, "gt-mr-unmerged-tip", false, "")
	if err == nil || !strings.Contains(err.Error(), "not preserved on origin/main") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want unpreserved-live-tip refusal", err)
	}
	if cleanup.RemoteDeleted || cleanup.LocalDeleted {
		t.Fatalf("cleanup claims a delete it did not do: %+v", cleanup)
	}
	if remote := runOrphanCleanupGit(t, clone, "ls-remote", "--heads", "origin", branch); remote == "" {
		t.Fatal("remote branch deleted despite carrying work that never landed")
	}
}

// initFastForwardedMRRepo builds the gt-fq4e shape in a real repository: a
// multi-commit branch, a target that moved underneath it, the queue's clean
// rebase of the branch onto that target, and the landing that leaves main
// holding the branch's own commits rather than a merge or squash of them —
// the fast-forward gt done's direct-merge convoy pushes. The MR bead's
// commit_sha still names the pre-rebase head, and landedCommits caps how much
// of the branch reaches main (fewer than the branch's commits = a partial
// landing). The returned origin is bare, so each test clones it.
func initFastForwardedMRRepo(t *testing.T, commits, landedCommits int) mrCleanupRepo {
	t.Helper()
	repo := mrCleanupRepo{branch: "polecat/quartz/gt-fq4e"}

	tmp := t.TempDir()
	repo.originPath = filepath.Join(tmp, "origin.git")
	runOrphanCleanupGit(t, tmp, "init", "--bare", "-b", "main", repo.originPath)

	clone := filepath.Join(tmp, "clone")
	runOrphanCleanupGit(t, tmp, "clone", repo.originPath, clone)
	runOrphanCleanupGit(t, clone, "config", "user.email", "polecat@example.com")
	runOrphanCleanupGit(t, clone, "config", "user.name", "Polecat Test")

	writeOrphanCleanupFile(t, clone, "seed.txt", "seed\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "seed main")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", "main")

	runOrphanCleanupGit(t, clone, "checkout", "-b", repo.branch)
	for i := 0; i < commits; i++ {
		// Alternate the file so no single commit carries the whole branch, as
		// in the reported 8-commit MR.
		name := "jsonl_git_backup.go"
		if i%2 == 1 {
			name = "jsonl_git_backup_test.go"
		}
		writeOrphanCleanupFile(t, clone, name, "change "+strconv.Itoa(i)+"\n")
		runOrphanCleanupGit(t, clone, "add", "-A")
		runOrphanCleanupGit(t, clone, "commit", "-m", "fix(daemon): change "+strconv.Itoa(i)+" (gt-fq4e)")
	}
	repo.submittedHead = runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", repo.branch)

	// The target moves underneath, so landing the branch means replaying it
	// onto the new tip.
	runOrphanCleanupGit(t, clone, "checkout", "main")
	writeOrphanCleanupFile(t, clone, "other.txt", "concurrent\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "concurrent target change")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")

	runOrphanCleanupGit(t, clone, "checkout", repo.branch)
	runOrphanCleanupGit(t, clone, "rebase", "main")
	runOrphanCleanupGit(t, clone, "push", "--force", "origin", repo.branch)

	landedTip := runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	if landedCommits < commits {
		// Only the first landedCommits commits of the rebased branch reach
		// main: the rest never land.
		landedTip = runOrphanCleanupGit(t, clone, "rev-parse", "HEAD~"+strconv.Itoa(commits-landedCommits))
	}
	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--ff-only", landedTip)
	repo.landedCommit = runOrphanCleanupGit(t, clone, "rev-parse", "HEAD")
	if landedCommits == commits && repo.landedCommit == runOrphanCleanupGit(t, clone, "rev-parse", "origin/main") {
		t.Fatalf("test premise broken: the branch landed as nothing new on main")
	}
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	// The retry that meets a deleted branch: the live-tip lookup falls back to
	// the recorded commit_sha, which the rebase left stale.
	runOrphanCleanupGit(t, clone, "push", "origin", "--delete", repo.branch)
	return repo
}

// TestVerifyMQPostMergeProof_RealFastForwardedMultiCommitBranch is the reported
// defect (gt-fq4e) against real git: 8 commits touching two files, no commit
// touching both, landed as the branch's own commits on main. The attestation
// has to satisfy the proof, and the error this test would have failed with is
// the exact one the report quotes — a submitted file absent from the attested
// commit's own changed files.
func TestVerifyMQPostMergeProof_RealFastForwardedMultiCommitBranch(t *testing.T) {
	t.Parallel()
	repo := initFastForwardedMRRepo(t, 8, 8)
	clone := cloneMRCleanupRepo(t, repo.originPath)
	g := git.NewGit(clone)

	mr := mrCleanupRequest(repo)
	mr.CommitSHA = repo.submittedHead
	recorded, err := verifyMQPostMergeProof(g, mr, mr.CommitSHA, repo.landedCommit)
	if err != nil {
		t.Fatalf("verifyMQPostMergeProof: %v", err)
	}
	if recorded != repo.landedCommit {
		t.Fatalf("recorded merge commit = %q, want the attested landed commit %q", recorded, repo.landedCommit)
	}
	// The landing really is the multi-commit shape: the attested commit's own
	// diff covers one of the two files, so only the patch-id binding can admit
	// it.
	ownFiles := runOrphanCleanupGit(t, clone, "diff", "--name-only", repo.landedCommit+"^", repo.landedCommit)
	if strings.Contains(ownFiles, "\n") {
		t.Fatalf("test premise broken: the attested commit already covers every file (%q)", ownFiles)
	}
}

// TestVerifyMQPostMergeProof_RealFastForwardedMultiCommitBranchRefusesPartialLanding
// is the fail-closed half (gt-fq4e): half the branch landed. The unlanded
// commits have no patch-id on main, and their files never appear in the
// attested commit's diff, so the proof must still refuse.
func TestVerifyMQPostMergeProof_RealFastForwardedMultiCommitBranchRefusesPartialLanding(t *testing.T) {
	t.Parallel()
	repo := initFastForwardedMRRepo(t, 4, 2)
	clone := cloneMRCleanupRepo(t, repo.originPath)
	g := git.NewGit(clone)

	mr := mrCleanupRequest(repo)
	mr.CommitSHA = repo.submittedHead
	_, err := verifyMQPostMergeProof(g, mr, mr.CommitSHA, repo.landedCommit)
	if err == nil {
		t.Fatal("merge proof accepted a landing that carried only half the submitted commits")
	}
	if !strings.Contains(err.Error(), "attestation does not match MR") {
		t.Fatalf("verifyMQPostMergeProof error = %v, want attestation-does-not-match-MR refusal", err)
	}
}

// TestRunVerifiedMQPostMerge_RealRemoteDirectlyCreatedMR is the incident the
// bead reports (gt-d7ir, gt-93m1), composed against real git: an MR created
// directly rather than through 'gt mq submit' records a branch but no
// commit_sha, and its work is nonetheless on main. Post-merge has to finish —
// the merge is already pushed at this point, so refusing leaves the MR open,
// the source issue open, and the branch undeleted around landed work.
func TestRunVerifiedMQPostMerge_RealRemoteDirectlyCreatedMR(t *testing.T) {
	t.Parallel()
	clone, branch := initOrphanCleanupRepo(t, true)
	g := orphanCleanupRealGit{git.NewGit(clone)}
	branchTip := runOrphanCleanupGit(t, clone, "rev-parse", "origin/"+branch)

	mgr := &fakeMQPostMergeManager{mr: &refinery.MergeRequest{
		ID:           "gt-mr-direct",
		Branch:       branch,
		Worker:       "polecats/operator",
		IssueID:      "gt-6o1u",
		TargetBranch: "main",
		// No commit_sha: the shape a directly-created MR bead has.
	}}
	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), g, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge for a directly-created MR: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called for a directly-created MR")
	}
	// The snapshot handed on keeps the bead's own (empty) commit_sha so the
	// close-time CAS compares against what the bead recorded; the tip the
	// proof bound against rides in VerifiedHead (gt-6o1u).
	if mgr.postMergeMR.CommitSHA != "" {
		t.Fatalf("snapshot commit_sha = %q, want empty (the bead records no commit_sha)", mgr.postMergeMR.CommitSHA)
	}
	if !mgr.postMergeMR.CommitSHAInferred {
		t.Fatalf("snapshot CommitSHAInferred = false, want true for a directly-created MR")
	}
	if mgr.postMergeMR.VerifiedHead != branchTip {
		t.Fatalf("snapshot VerifiedHead = %q, want the branch tip %q the proof bound against",
			mgr.postMergeMR.VerifiedHead, branchTip)
	}
	if cleanup.SubmittedHead != branchTip || !cleanup.SubmittedHeadInferred {
		t.Fatalf("cleanup = %+v, want the inferred branch tip %s recorded", cleanup, branchTip)
	}
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the remote branch deleted", cleanup)
	}
	if remote := runOrphanCleanupGit(t, clone, "ls-remote", "--heads", "origin", branch); remote != "" {
		t.Fatalf("remote branch survived cleanup: %s", remote)
	}
}

// TestRunVerifiedMQPostMerge_RealRemoteFastForwardedDirectlyCreatedMR is the
// regression for the vacuous-proof guard refusing every real fast-forward
// landing (om review, attempt 3): after a fast-forward, an inferred head is
// always an ancestor of target — that is what a fast-forward landing is — so
// a guard scoped to target's whole history would refuse this MR exactly as it
// refuses a branch that was never touched. Scoping the guard to target's
// state one commit before its current tip must still close this MR, since
// the branch's own commit is what advanced target past that point.
func TestRunVerifiedMQPostMerge_RealRemoteFastForwardedDirectlyCreatedMR(t *testing.T) {
	t.Parallel()
	clone, branch := initOrphanCleanupRepo(t, false)
	branchTip := runOrphanCleanupGit(t, clone, "rev-parse", "origin/"+branch)
	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--ff-only", branch)
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	g := orphanCleanupRealGit{git.NewGit(clone)}

	mgr := &fakeMQPostMergeManager{mr: &refinery.MergeRequest{
		ID:           "gt-mr-ff-direct",
		Branch:       branch,
		Worker:       "polecats/operator",
		IssueID:      "gt-6o1u",
		TargetBranch: "main",
		// No commit_sha: the shape a directly-created MR bead has.
	}}
	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), g, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge for a fast-forwarded directly-created MR: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called for a fast-forwarded directly-created MR")
	}
	if mgr.postMergeMR.VerifiedHead != branchTip {
		t.Fatalf("snapshot VerifiedHead = %q, want the branch tip %q the proof bound against",
			mgr.postMergeMR.VerifiedHead, branchTip)
	}
	if cleanup.SubmittedHead != branchTip || !cleanup.SubmittedHeadInferred {
		t.Fatalf("cleanup = %+v, want the inferred branch tip %s recorded", cleanup, branchTip)
	}
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the remote branch deleted", cleanup)
	}
}

// TestRunVerifiedMQPostMerge_RealRemoteMergeCommitDirectlyCreatedMR is the
// merge-commit half of the same regression: after landing by a merge commit
// (not a fast-forward), the branch's own tip is a parent of target's new tip,
// not the tip itself, but it is still an ancestor of target's whole history —
// the same shape a vacuous branch has. The guard must tell them apart by
// target's state one commit before its current tip too.
func TestRunVerifiedMQPostMerge_RealRemoteMergeCommitDirectlyCreatedMR(t *testing.T) {
	t.Parallel()
	clone, branch := initOrphanCleanupRepo(t, false)
	branchTip := runOrphanCleanupGit(t, clone, "rev-parse", "origin/"+branch)
	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--no-ff", "-m", "merge branch", branch)
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	g := orphanCleanupRealGit{git.NewGit(clone)}

	mgr := &fakeMQPostMergeManager{mr: &refinery.MergeRequest{
		ID:           "gt-mr-merge-direct",
		Branch:       branch,
		Worker:       "polecats/operator",
		IssueID:      "gt-6o1u",
		TargetBranch: "main",
		// No commit_sha: the shape a directly-created MR bead has.
	}}
	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), g, mgr.mr.ID, false, "")
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge for a merge-commit directly-created MR: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called for a merge-commit directly-created MR")
	}
	if mgr.postMergeMR.VerifiedHead != branchTip {
		t.Fatalf("snapshot VerifiedHead = %q, want the branch tip %q the proof bound against",
			mgr.postMergeMR.VerifiedHead, branchTip)
	}
	if cleanup.SubmittedHead != branchTip || !cleanup.SubmittedHeadInferred {
		t.Fatalf("cleanup = %+v, want the inferred branch tip %s recorded", cleanup, branchTip)
	}
	if !cleanup.RemoteDeleted {
		t.Fatalf("cleanup = %+v, want the remote branch deleted", cleanup)
	}
}

// TestRunVerifiedMQPostMerge_InferredHeadWithLandedCommitAttestation is the
// intersection the bead's report calls out by hand: '--landed-commit could not
// substitute because that flag only overrides the LANDED sha, not the SUBMITTED
// one'. Once the head is resolved from the branch, the attestation has
// something to bind against and the directly-created MR closes.
func TestRunVerifiedMQPostMerge_InferredHeadWithLandedCommitAttestation(t *testing.T) {
	t.Parallel()
	const tip = "b1668c652c0073be141070ef5228c2edc4357825"
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		remoteTip:      tip,
		localHead:      tip,
		targetPriorTip: "oldmain000oldmain000oldmain000oldmain000",
		mergeBase:      "oldbase000",
		landedParent:   "oldmain111",
		submittedFiles: []string{"internal/cmd/mq.go"},
		landedFiles:    []string{"internal/cmd/mq.go"},
	}
	const landedCommit = "e23984a7e23984a7e23984a7e23984a7e23984a7"

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true, landedCommit)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge with --landed-commit on a directly-created MR: %v", err)
	}
	// mergeBaseSubmits[0] is the vacuous-proof guard's merge-base (always for
	// an inferred head); [1] is the attestation binding itself.
	if len(rigGit.mergeBaseSubmits) != 2 || rigGit.mergeBaseSubmits[1] != tip {
		t.Fatalf("attestation bound against %v, want the inferred head %s as the second merge-base", rigGit.mergeBaseSubmits, tip)
	}
	if mgr.postMergeMR.MergeCommit != landedCommit {
		t.Fatalf("MR MergeCommit = %q, want the attested landed commit %q", mgr.postMergeMR.MergeCommit, landedCommit)
	}
}

// TestRunVerifiedMQPostMerge_RealInferredHeadRefusesUnmergedBranch is the
// fail-closed control for inference, against real git: the branch exists, so a
// head is resolvable, but its work is not on main. Inference decides which
// commit is proven, not whether it has to be there — if it could close an MR
// whose content never landed it would be a hole, not a recovery.
func TestRunVerifiedMQPostMerge_RealInferredHeadRefusesUnmergedBranch(t *testing.T) {
	t.Parallel()
	clone, branch := initOrphanCleanupRepo(t, false)
	g := orphanCleanupRealGit{git.NewGit(clone)}

	mgr := &fakeMQPostMergeManager{mr: &refinery.MergeRequest{
		ID:           "gt-mr-never-landed",
		Branch:       branch,
		TargetBranch: "main",
		// No commit_sha, as a directly-created MR has none.
	}}
	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), g, mgr.mr.ID, false, "")
	if err == nil || !strings.Contains(err.Error(), "does not contain submitted head") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want the target-does-not-contain refusal", err)
	}
	if !strings.Contains(err.Error(), "inferred from branch") {
		t.Fatalf("proof error %q does not say the head was inferred", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called for an inferred head that is not on the target")
	}
	if cleanup.RemoteDeleted || cleanup.LocalDeleted {
		t.Fatalf("cleanup claims a delete it did not do: %+v", cleanup)
	}
	if remote := runOrphanCleanupGit(t, clone, "ls-remote", "--heads", "origin", branch); remote == "" {
		t.Fatal("remote branch deleted despite carrying work that never landed")
	}
}

// TestMQPostMergeSilencesUsageOnError pins the messenger side of the bead:
// cobra appends the failing command's usage block to any RunE error, which in
// the reported incident put two screens of flags between the operator and the
// refusal that actually mattered.
//
// It reads the flag cobra consults rather than driving the command, because
// ExecuteC on a command with a parent re-dispatches through the real root, and
// root's persistentPreRun touches heartbeat files and the session registry —
// side effects no unit test should cause.
func TestMQPostMergeSilencesUsageOnError(t *testing.T) {
	t.Parallel()
	if !mqPostMergeCmd.SilenceUsage {
		t.Fatal("mq post-merge prints its usage block after an operational error; see SilenceUsage on doneCmd")
	}
}
