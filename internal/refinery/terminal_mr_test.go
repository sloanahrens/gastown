package refinery

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

func TestValidateTerminalMRCloseSnapshotRejectsDrift(t *testing.T) {
	t.Parallel()
	expected := &MergeRequest{
		ID:           "gt-mr-proof",
		Branch:       "polecat/test/proof",
		IssueID:      "gt-proof",
		TargetBranch: "main",
		CommitSHA:    "abc123",
	}
	fields := &beads.MRFields{
		Branch:      "polecat/test/proof",
		SourceIssue: "gt-proof",
		Target:      "main",
		CommitSHA:   "def456",
	}

	err := validateTerminalMRCloseSnapshot(expected.ID, fields, expected)
	if err == nil || !strings.Contains(err.Error(), "changed after merge proof") {
		t.Fatalf("validateTerminalMRCloseSnapshot error = %v, want drift failure", err)
	}
}

func TestValidateTerminalMRCloseSnapshotAllowsMatchingSnapshot(t *testing.T) {
	t.Parallel()
	expected := &MergeRequest{
		ID:           "gt-mr-proof",
		Branch:       "polecat/test/proof",
		IssueID:      "gt-proof",
		TargetBranch: "main",
		CommitSHA:    "abc123",
	}
	fields := &beads.MRFields{
		Branch:      "polecat/test/proof",
		SourceIssue: "gt-proof",
		Target:      "main",
		CommitSHA:   "abc123",
	}

	if err := validateTerminalMRCloseSnapshot(expected.ID, fields, expected); err != nil {
		t.Fatalf("validateTerminalMRCloseSnapshot error = %v, want nil for a matching snapshot", err)
	}
}

// testStoreBeads wraps an in-process store in the beads.Beads layer
// closeTerminalMR operates through. batchReviewStore (batch_editorial_test.go)
// already implements the Storage surface this exercises — GetIssue,
// UpdateIssue, CloseIssue, GetLabels, GetDependenciesWithMetadata — so the
// tests below drive the real close path, not a fake manager. The work dir is a
// temp dir so the agent-bead lock (a file under the beads dir) has somewhere to
// live.
func testStoreBeads(t *testing.T, store beadsdk.Storage) *beads.Beads {
	t.Helper()
	return beads.NewWithStore(t.TempDir(), store)
}

func terminalMRIssue(id, branch, target, sourceIssue, commitSHA string) *beadsdk.Issue {
	fields := &beads.MRFields{
		Branch:      branch,
		Target:      target,
		SourceIssue: sourceIssue,
		CommitSHA:   commitSHA,
	}
	now := time.Now()
	return &beadsdk.Issue{
		ID:          id,
		Title:       id,
		Description: beads.FormatMRFields(fields),
		Status:      beadsdk.StatusOpen,
		IssueType:   beadsdk.IssueType("task"),
		Priority:    2,
		CreatedAt:   now,
		UpdatedAt:   now,
		Labels:      []string{"gt:merge-request"},
	}
}

// TestCloseTerminalMR_RecordsInferredHead exercises the real close path for
// the gt-6o1u case: an MR whose bead recorded no commit_sha. The close must
// succeed and persist the verified inferred head with commit_sha_inferred
// flagged, so the record names the head the merge proof actually bound
// against — while a later re-run sees a non-empty commit_sha and therefore
// never re-infers.
func TestCloseTerminalMR_RecordsInferredHead(t *testing.T) {
	t.Parallel()
	const (
		mrID    = "gt-mr-direct"
		branch  = "polecat/test/direct"
		issueID = "gt-direct"
	)
	inferredHead := "deadbeef1234deadbeef1234deadbeef1234deadbeef"

	store := newBatchReviewStore(terminalMRIssue(mrID, branch, "main", issueID, ""))
	b := testStoreBeads(t, store)

	expected := &MergeRequest{
		ID:                mrID,
		Branch:            branch,
		IssueID:           issueID,
		TargetBranch:      "main",
		CommitSHA:         "", // pristine snapshot: the bead recorded nothing
		CommitSHAInferred: true,
		VerifiedHead:      inferredHead,
	}
	opts := terminalMRCloseOptions{
		Reason:            "merged",
		MergeCommit:       "merge000merge000merge000merge000merge000",
		ExpectedMR:        expected,
		InferredCommitSHA: inferredHead,
	}

	result, err := closeTerminalMR(b, mrID, opts)
	if err != nil {
		t.Fatalf("closeTerminalMR: %v", err)
	}
	if !result.Closed {
		t.Fatalf("result.Closed = false, want true")
	}

	after, err := store.GetIssue(context.Background(), mrID)
	if err != nil {
		t.Fatalf("GetIssue after close: %v", err)
	}
	if after.Status != beadsdk.StatusClosed {
		t.Fatalf("status = %q, want closed", after.Status)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: after.Description})
	if fields == nil {
		t.Fatalf("ParseMRFields: no MR fields in %q", after.Description)
	}
	if fields.CommitSHA != inferredHead {
		t.Fatalf("close recorded commit_sha = %q, want the verified inferred head %q", fields.CommitSHA, inferredHead)
	}
	if !fields.CommitSHAInferred {
		t.Fatalf("close recorded commit_sha_inferred = false, want true")
	}
	if fields.MergeCommit == "" {
		t.Fatalf("close recorded no merge_commit")
	}

	if result.AlreadyTerminal {
		t.Fatalf("first close: AlreadyTerminal = true, want false (fresh close)")
	}
}

// TestCloseTerminalMR_ReopenIsIdempotent pins the recovery re-runnability
// contract for an MR whose close recorded the inferred head: after reopening
// the same bead (a prior close that lost the race at the status transition),
// the next close must pass its CAS and close it again instead of refusing as
// divergent. The second call models what runVerifiedMQPostMerge actually
// sends on a re-run: the bead now has commit_sha set, so inference does not
// run again (TestRunVerifiedMQPostMerge_RecordedInferredHeadIsNotReInferred),
// and the close goes through the regular commit_sha CAS, not the inferred one
// — InferredCommitSHA is empty and ExpectedMR.CommitSHA already carries the
// recorded head.
func TestCloseTerminalMR_ReopenIsIdempotent(t *testing.T) {
	t.Parallel()
	const (
		mrID    = "gt-mr-direct"
		branch  = "polecat/test/direct"
		issueID = "gt-direct"
	)
	inferredHead := "deadbeef1234deadbeef1234deadbeef1234deadbeef"

	store := newBatchReviewStore(terminalMRIssue(mrID, branch, "main", issueID, ""))
	b := testStoreBeads(t, store)

	expected := &MergeRequest{
		ID:                mrID,
		Branch:            branch,
		IssueID:           issueID,
		TargetBranch:      "main",
		CommitSHA:         "", // pristine snapshot: the bead recorded nothing
		CommitSHAInferred: true,
		VerifiedHead:      inferredHead,
	}
	opts := terminalMRCloseOptions{
		Reason:            "merged",
		MergeCommit:       "merge000merge000merge000merge000merge000",
		ExpectedMR:        expected,
		InferredCommitSHA: inferredHead,
	}

	first, err := closeTerminalMR(b, mrID, opts)
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	if !first.Closed {
		t.Fatalf("first close: result.Closed = false, want true")
	}

	// Reopen the bead. The batchReviewStore keeps issues in a map and hands
	// out the shared pointer, so the status change persists on that pointer
	// (but any UpdateIssue write-back would clobber it — hence a fresh
	// store for the re-close).
	reopen, err := store.GetIssue(context.Background(), mrID)
	if err != nil {
		t.Fatalf("GetIssue before reopen: %v", err)
	}
	reopen.Status = beadsdk.StatusOpen
	reopened := newBatchReviewStore(reopen)
	rb := testStoreBeads(t, reopened)

	// A real re-run re-fetches the bead: commit_sha is now recorded, so
	// inference does not fire and the snapshot it hands to close names the
	// recorded head directly rather than an inferred one.
	rerunExpected := &MergeRequest{
		ID:                mrID,
		Branch:            branch,
		IssueID:           issueID,
		TargetBranch:      "main",
		CommitSHA:         inferredHead,
		CommitSHAInferred: true,
	}
	rerunOpts := terminalMRCloseOptions{
		Reason:      "merged",
		MergeCommit: "merge000merge000merge000merge000merge000",
		ExpectedMR:  rerunExpected,
	}

	reResult, err := closeTerminalMR(rb, mrID, rerunOpts)
	if err != nil {
		t.Fatalf("re-close of a recorded-inferred-head MR: %v (must be idempotent, not a divergence refusal)", err)
	}
	if reResult.AlreadyTerminal {
		t.Fatalf("re-close: AlreadyTerminal = true, want false (reopened bead closed fresh)")
	}
	if !reResult.Closed {
		t.Fatalf("re-close: result.Closed = false, want true")
	}

	// The recorded head must survive the re-close unchanged — the close
	// records the verified head, never re-infers.
	after, err := reopened.GetIssue(context.Background(), mrID)
	if err != nil {
		t.Fatalf("GetIssue after re-close: %v", err)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: after.Description})
	if fields == nil {
		t.Fatalf("ParseMRFields: no MR fields in %q", after.Description)
	}
	if fields.CommitSHA != inferredHead {
		t.Fatalf("re-close recorded commit_sha = %q, want the unchanged inferred head %q", fields.CommitSHA, inferredHead)
	}
}

// TestCloseTerminalMR_RefusesInferredHeadDivergence is the fail-closed half of
// the same contract: if the bead (or a concurrent writer) recorded a commit_sha
// that differs from the verified inferred head, the close is refused — the
// divergence surfaces instead of being papered over by the close's fill.
func TestCloseTerminalMR_RefusesInferredHeadDivergence(t *testing.T) {
	t.Parallel()
	const (
		mrID    = "gt-mr-direct"
		branch  = "polecat/test/direct"
		issueID = "gt-direct"
	)
	divergent := "0000aaaa11110000aaaa11110000aaaa1111"
	inferredHead := "deadbeef1234deadbeef1234deadbeef1234deadbeef"

	// The bead recorded a DIFFERENT commit than the one the merge proof bound
	// against — a race between inference and a writer that filled commit_sha.
	store := newBatchReviewStore(terminalMRIssue(mrID, branch, "main", issueID, divergent))
	b := testStoreBeads(t, store)

	expected := &MergeRequest{
		ID:                mrID,
		Branch:            branch,
		IssueID:           issueID,
		TargetBranch:      "main",
		CommitSHA:         "", // the pre-close snapshot saw no commit_sha
		CommitSHAInferred: true,
		VerifiedHead:      inferredHead,
	}
	opts := terminalMRCloseOptions{
		Reason:            "merged",
		ExpectedMR:        expected,
		InferredCommitSHA: inferredHead,
	}

	_, err := closeTerminalMR(b, mrID, opts)
	if err == nil {
		t.Fatalf("closeTerminalMR succeeded, want refusal for a divergent recorded commit_sha")
	}
	if !strings.Contains(err.Error(), "differs from the verified inferred head") {
		t.Fatalf("closeTerminalMR error = %v, want the inferred-head divergence message", err)
	}

	// The bead must remain open: a refused close closes nothing.
	after, gerr := store.GetIssue(context.Background(), mrID)
	if gerr != nil {
		t.Fatalf("GetIssue after refused close: %v", gerr)
	}
	if after.Status != beadsdk.StatusOpen {
		t.Fatalf("status = %q after a refused close, want open", after.Status)
	}
}

// TestCloseTerminalMR_InferredMarkerRequiresSnapshotInferred pins the
// attestation semantics: the inferred-head fill must NOT fire for a plain
// (non-inferred) snapshot even when InferredCommitSHA is passed. Without the
// guard, a caller that sets the option for one MR could silently rewrite a
// different MR's recorded commit_sha.
func TestCloseTerminalMR_InferredMarkerRequiresSnapshotInferred(t *testing.T) {
	t.Parallel()
	const (
		mrID    = "gt-mr-plain"
		branch  = "polecat/test/plain"
		issueID = "gt-plain"
	)
	recorded := "aaaabbbbccccddddeeeeaaaabbbbccccddddeeee"
	inferredHead := "deadbeef1234deadbeef1234deadbeef1234deadbeef"

	store := newBatchReviewStore(terminalMRIssue(mrID, branch, "main", issueID, recorded))
	b := testStoreBeads(t, store)

	// ExpectedMR is NOT marked inferred — the recorded commit_sha is the
	// submission identity and the regular snapshot CAS is the right tool.
	expected := &MergeRequest{
		ID:           mrID,
		Branch:       branch,
		IssueID:      issueID,
		TargetBranch: "main",
		CommitSHA:    recorded,
	}
	opts := terminalMRCloseOptions{
		Reason:            "merged",
		ExpectedMR:        expected,
		InferredCommitSHA: inferredHead,
	}

	if _, err := closeTerminalMR(b, mrID, opts); err != nil {
		t.Fatalf("closeTerminalMR: %v", err)
	}

	after, err := store.GetIssue(context.Background(), mrID)
	if err != nil {
		t.Fatalf("GetIssue after close: %v", err)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: after.Description})
	if fields == nil {
		t.Fatalf("ParseMRFields: no MR fields in %q", after.Description)
	}
	if fields.CommitSHA != recorded {
		t.Fatalf("close rewrote commit_sha = %q for a non-inferred snapshot, want the recorded %q untouched", fields.CommitSHA, recorded)
	}
	if fields.CommitSHAInferred {
		t.Fatalf("close set commit_sha_inferred = true for a non-inferred snapshot; the marker must stay false")
	}
}

// TestManager_postMergeMR_RecordsInferredHeadOnBead exercises the glue
// closeTerminalMR's own tests do not reach: Manager.postMergeMR is what turns
// mr.CommitSHAInferred/VerifiedHead into the InferredCommitSHA option that
// actually gets the head persisted (gt-6o1u). Calling the unexported method
// directly against an in-process store avoids the Dolt container the
// PostMerge/PostMergeMR-level tests in manager_test.go require.
func TestManager_postMergeMR_RecordsInferredHeadOnBead(t *testing.T) {
	t.Parallel()
	const (
		mrID    = "gt-mr-direct"
		branch  = "polecat/test/direct"
		issueID = "gt-direct"
	)
	inferredHead := "deadbeef1234deadbeef1234deadbeef1234deadbeef"

	store := newBatchReviewStore(terminalMRIssue(mrID, branch, "main", issueID, ""))
	b := testStoreBeads(t, store)
	m := &Manager{output: io.Discard}

	mr := &MergeRequest{
		ID:                mrID,
		Branch:            branch,
		IssueID:           issueID,
		TargetBranch:      "main",
		CommitSHA:         "", // pristine snapshot: the bead recorded nothing
		CommitSHAInferred: true,
		VerifiedHead:      inferredHead,
	}

	result, err := m.postMergeMR(b, mr)
	if err != nil {
		t.Fatalf("postMergeMR: %v", err)
	}
	if !result.MRClosed {
		t.Fatalf("result.MRClosed = false, want true")
	}

	after, err := store.GetIssue(context.Background(), mrID)
	if err != nil {
		t.Fatalf("GetIssue after postMergeMR: %v", err)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: after.Description})
	if fields == nil {
		t.Fatalf("ParseMRFields: no MR fields in %q", after.Description)
	}
	if fields.CommitSHA != inferredHead {
		t.Fatalf("postMergeMR recorded commit_sha = %q, want the verified inferred head %q", fields.CommitSHA, inferredHead)
	}
	if !fields.CommitSHAInferred {
		t.Fatalf("postMergeMR recorded commit_sha_inferred = false, want true")
	}
}
