package refinery

import (
	"bytes"
	"strings"
	"testing"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

// conflictMRBead builds an MR bead carrying the conflict-task link and the SHA
// recorded at submission time, as recordConflictTaskOnMR leaves it.
func conflictMRBead(id, branch, target, sourceIssue, commitSHA, taskID string) *beadsdk.Issue {
	return prepushIssue(id, beads.FormatMRFields(&beads.MRFields{
		Branch:         branch,
		Target:         target,
		SourceIssue:    sourceIssue,
		Worker:         "polecats/test",
		Rig:            "test-rig",
		CommitSHA:      commitSHA,
		ConflictTaskID: taskID,
		RetryCount:     1,
	}), "gt:merge-request")
}

// conflictTaskBead builds a conflict-resolution task of the shape
// createConflictResolutionTaskForMR produces. isConflictTaskForMR verifies the
// "Original MR" metadata line, so the description must carry it.
func conflictTaskBead(id, mrID, sourceIssue, branch, target string, closed bool) *beadsdk.Issue {
	desc := "Resolve merge conflicts for branch " + branch + "\n\n## Metadata\n" +
		"- Original MR: " + mrID + "\n" +
		"- Branch: " + branch + "\n" +
		"- Conflict with: " + target + "@deadbeef\n" +
		"- Original issue: " + sourceIssue + "\n" +
		"- Retry count: 1\n"
	task := prepushIssue(id, desc, "gt:task")
	if closed {
		task.Status = beadsdk.StatusClosed
	}
	return task
}

// resolveConflictOnBranch checks out branch, commits a change, and returns the
// SHA the branch moved to — the head a polecat pushes when resolving a conflict.
func resolveConflictOnBranch(t *testing.T, workDir, branch string) (before, after string) {
	t.Helper()
	before = run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)
	run(t, workDir, "git", "checkout", branch)
	writeFile(t, workDir, "resolution.txt", "conflict resolved\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "Merge origin/main and resolve conflicts")
	after = run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)
	run(t, workDir, "git", "checkout", "main")
	if before == after {
		t.Fatal("test setup: resolution did not move the branch head")
	}
	return before, after
}

func mrFieldsFromStore(t *testing.T, store *prepushStore, id string) *beads.MRFields {
	t.Helper()
	issue, ok := store.issues[id]
	if !ok {
		t.Fatalf("no issue %s in store", id)
	}
	fields := beads.ParseMRFields(&beads.Issue{Description: issue.Description})
	if fields == nil {
		t.Fatalf("no parsable MR fields in %s description: %q", id, issue.Description)
	}
	return fields
}

// TestSubmittedBranchHead_AdoptsHeadAfterConflictResolution is the gt-pwa1
// regression: the documented conflict-resolution flow (push the resolved
// branch, close the conflict task) must let the MR retry. Before the fix the
// MR bead kept the pre-conflict commit_sha, so submittedBranchHead rejected the
// retry with "source branch X changed from submitted head A to B" forever.
func TestSubmittedBranchHead_AdoptsHeadAfterConflictResolution(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/jade/gt-fo3h"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	submitted, resolved := resolveConflictOnBranch(t, workDir, branch)

	store := newPrepushStore(
		conflictMRBead("gt-mr-1", branch, "main", "gt-fo3h", submitted, "gt-task-1"),
		conflictTaskBead("gt-task-1", "gt-mr-1", "gt-fo3h", branch, "main", true),
	)
	e := newPrepushEngineer(t, workDir, store)

	mr := &MRInfo{
		ID: "gt-mr-1", Branch: branch, Target: "main", SourceIssue: "gt-fo3h",
		CommitSHA: submitted, ConflictTaskID: "gt-task-1",
	}
	head, err := e.submittedBranchHead(mr)
	if err != nil {
		t.Fatalf("submittedBranchHead after conflict resolution: %v", err)
	}
	if head != resolved {
		t.Errorf("submitted head = %s, want resolved head %s", head, resolved)
	}
	if mr.CommitSHA != resolved {
		t.Errorf("in-memory CommitSHA = %s, want %s", mr.CommitSHA, resolved)
	}
	if mr.ConflictTaskID != "" {
		t.Errorf("in-memory ConflictTaskID = %q, want cleared", mr.ConflictTaskID)
	}

	fields := mrFieldsFromStore(t, store, "gt-mr-1")
	if fields.CommitSHA != resolved {
		t.Errorf("persisted commit_sha = %s, want %s", fields.CommitSHA, resolved)
	}
	if fields.ConflictTaskID != "" {
		t.Errorf("persisted conflict_task_id = %q, want cleared", fields.ConflictTaskID)
	}
	if fields.RetryCount != 1 {
		t.Errorf("persisted retry_count = %d, want 1 (history preserved)", fields.RetryCount)
	}
}

// TestSubmittedBranchHead_RejectsMovedHeadWhileConflictTaskOpen guards the other
// half of the contract: the task closing is what authorizes the head to have
// moved. While it is still open the strict comparison must still fire.
func TestSubmittedBranchHead_RejectsMovedHeadWhileConflictTaskOpen(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/jade/gt-open"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	submitted, _ := resolveConflictOnBranch(t, workDir, branch)

	store := newPrepushStore(
		conflictMRBead("gt-mr-2", branch, "main", "gt-open", submitted, "gt-task-2"),
		conflictTaskBead("gt-task-2", "gt-mr-2", "gt-open", branch, "main", false),
	)
	e := newPrepushEngineer(t, workDir, store)

	mr := &MRInfo{
		ID: "gt-mr-2", Branch: branch, Target: "main", SourceIssue: "gt-open",
		CommitSHA: submitted, ConflictTaskID: "gt-task-2",
	}
	_, err := e.submittedBranchHead(mr)
	if err == nil {
		t.Fatal("expected rejection while the conflict task is still open")
	}
	if !strings.Contains(err.Error(), "changed from submitted head") {
		t.Errorf("error = %v, want a changed-head rejection", err)
	}
	if got := mrFieldsFromStore(t, store, "gt-mr-2").CommitSHA; got != submitted {
		t.Errorf("persisted commit_sha = %s, want it left at %s", got, submitted)
	}
}

// TestSubmittedBranchHead_RefusesUnverifiedConflictTask covers the authorization
// boundary: a closed task that does not belong to this MR must not unlock the
// head guard, however the conflict_task_id field came to be set.
func TestSubmittedBranchHead_RefusesUnverifiedConflictTask(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/jade/gt-wrong-mr"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	submitted, _ := resolveConflictOnBranch(t, workDir, branch)

	store := newPrepushStore(
		conflictMRBead("gt-mr-3", branch, "main", "gt-wrong-mr", submitted, "gt-task-3"),
		// Belongs to a different MR.
		conflictTaskBead("gt-task-3", "gt-mr-other", "gt-wrong-mr", branch, "main", true),
	)
	e := newPrepushEngineer(t, workDir, store)
	log := e.output.(*bytes.Buffer)

	mr := &MRInfo{
		ID: "gt-mr-3", Branch: branch, Target: "main", SourceIssue: "gt-wrong-mr",
		CommitSHA: submitted, ConflictTaskID: "gt-task-3",
	}
	if _, err := e.submittedBranchHead(mr); err == nil {
		t.Fatal("expected rejection: closed task belongs to a different MR")
	}
	if got := mrFieldsFromStore(t, store, "gt-mr-3").CommitSHA; got != submitted {
		t.Errorf("persisted commit_sha = %s, want it left at %s", got, submitted)
	}
	if !strings.Contains(log.String(), "not a verified conflict task") {
		t.Errorf("expected an explicit refusal in the log, got %q", log.String())
	}
}

// TestSubmittedBranchHead_UnmovedBranchClearsSpentTaskLink covers the no-op
// path: a closed conflict task whose branch head never moved (resolution merged
// nothing new) should still drop the spent link so later cycles short-circuit.
func TestSubmittedBranchHead_UnmovedBranchClearsSpentTaskLink(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/jade/gt-same"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	head := run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)

	store := newPrepushStore(
		conflictMRBead("gt-mr-4", branch, "main", "gt-same", head, "gt-task-4"),
		conflictTaskBead("gt-task-4", "gt-mr-4", "gt-same", branch, "main", true),
	)
	e := newPrepushEngineer(t, workDir, store)

	mr := &MRInfo{
		ID: "gt-mr-4", Branch: branch, Target: "main", SourceIssue: "gt-same",
		CommitSHA: head, ConflictTaskID: "gt-task-4",
	}
	got, err := e.submittedBranchHead(mr)
	if err != nil {
		t.Fatalf("submittedBranchHead: %v", err)
	}
	if got != head {
		t.Errorf("submitted head = %s, want %s", got, head)
	}
	if fields := mrFieldsFromStore(t, store, "gt-mr-4"); fields.ConflictTaskID != "" {
		t.Errorf("persisted conflict_task_id = %q, want cleared", fields.ConflictTaskID)
	}
}

// TestSubmittedBranchHead_NoConflictTaskKeepsStrictGuard is the plain-MR
// regression: MRs that never hit a conflict keep the original guard exactly.
func TestSubmittedBranchHead_NoConflictTaskKeepsStrictGuard(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/jade/gt-plain"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	submitted, _ := resolveConflictOnBranch(t, workDir, branch)

	store := newPrepushStore(
		conflictMRBead("gt-mr-5", branch, "main", "gt-plain", submitted, ""),
	)
	e := newPrepushEngineer(t, workDir, store)

	mr := &MRInfo{
		ID: "gt-mr-5", Branch: branch, Target: "main", SourceIssue: "gt-plain",
		CommitSHA: submitted,
	}
	_, err := e.submittedBranchHead(mr)
	if err == nil {
		t.Fatal("expected rejection: a non-conflict MR whose branch moved must not be merged")
	}
	if !strings.Contains(err.Error(), "changed from submitted head") {
		t.Errorf("error = %v, want a changed-head rejection", err)
	}
}

// TestSubmittedBranchHead_AdoptsViaDependencyWhenFieldBlank is the gt-8cre7
// regression: conflict_task_id can go blank on the MR bead (e.g. an earlier
// cycle's head==recorded short-circuit spent it before the resolved push it
// announced actually landed) while the dependency edge the conflict task was
// created under still verifies. Adoption must recover from that dependency
// rather than treating a blank field as "no conflict ever happened".
func TestSubmittedBranchHead_AdoptsViaDependencyWhenFieldBlank(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/jade/gt-blank-field"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	submitted, resolved := resolveConflictOnBranch(t, workDir, branch)

	// conflict_task_id is blank on the MR bead, but retry_count carries the
	// conflict history and a closed dependency still names this MR.
	store := newPrepushStore(
		conflictMRBead("gt-mr-7", branch, "main", "gt-blank-field", submitted, ""),
		conflictTaskBead("gt-task-7", "gt-mr-7", "gt-blank-field", branch, "main", true),
	)
	store.deps = map[string][]*beadsdk.IssueWithDependencyMetadata{
		"gt-mr-7": {{
			Issue:          beadsdk.Issue{ID: "gt-task-7", Status: beadsdk.StatusClosed},
			DependencyType: beadsdk.DepBlocks,
		}},
	}
	e := newPrepushEngineer(t, workDir, store)

	mr := &MRInfo{
		ID: "gt-mr-7", Branch: branch, Target: "main", SourceIssue: "gt-blank-field",
		CommitSHA: submitted, RetryCount: 1,
	}
	head, err := e.submittedBranchHead(mr)
	if err != nil {
		t.Fatalf("submittedBranchHead with blank conflict_task_id: %v", err)
	}
	if head != resolved {
		t.Errorf("submitted head = %s, want resolved head %s", head, resolved)
	}

	fields := mrFieldsFromStore(t, store, "gt-mr-7")
	if fields.CommitSHA != resolved {
		t.Errorf("persisted commit_sha = %s, want %s", fields.CommitSHA, resolved)
	}
}

// TestSubmittedBranchHead_BlankFieldNoRetryHistoryKeepsStrictGuard covers the
// cheap early-out: an MR that never recorded a conflict retry (RetryCount==0)
// must not pay for a dependency scan and must keep the strict comparison,
// even if some unrelated closed dependency happens to exist.
func TestSubmittedBranchHead_BlankFieldNoRetryHistoryKeepsStrictGuard(t *testing.T) {
	t.Parallel()
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const branch = "polecat/jade/gt-no-history"
	createFeatureBranch(t, workDir, branch, "feature.txt", "v1\n")
	submitted, _ := resolveConflictOnBranch(t, workDir, branch)

	store := newPrepushStore(
		prepushMRIssue("gt-mr-8", branch, "main", "gt-no-history", submitted),
	)
	e := newPrepushEngineer(t, workDir, store)

	mr := &MRInfo{
		ID: "gt-mr-8", Branch: branch, Target: "main", SourceIssue: "gt-no-history",
		CommitSHA: submitted,
	}
	if _, err := e.submittedBranchHead(mr); err == nil {
		t.Fatal("expected rejection: no conflict retry history must not unlock adoption")
	}
}

// TestConflictTaskInstructionsDocumentPushAndClose pins the contract the fix
// makes true: the task text has to tell the polecat to do both steps, since a
// push without a close leaves the MR blocked and a close without a push leaves
// the merge failing against the old head.
func TestConflictTaskInstructionsDocumentPushAndClose(t *testing.T) {
	t.Parallel()

	desc := conflictTaskDescription(&MRInfo{ID: "gt-mr-6"}, "polecat/jade/gt-x", "main", "deadbeef", 1)
	for _, want := range []string{
		"git push origin polecat/jade/gt-x",
		"bd close <this-task-id>",
		"Both steps are required",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("conflict task description missing %q:\n%s", want, desc)
		}
	}
}
