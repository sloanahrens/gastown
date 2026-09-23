package refinery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dirtyStagedFile stages a new file in the worktree — the exact shape the
// 2026-09-18 incident left in the refinery rig (gt-nnwy).
func dirtyStagedFile(t *testing.T, workDir, name string) {
	t.Helper()
	writeFile(t, workDir, name, "package cmd\n")
	run(t, workDir, "git", "add", name)
}

func TestAssertWorktreeNotExternallyDirty_RefusesStagedChanges(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	dirtyStagedFile(t, workDir, "slot_run_nice_test.go")

	err := e.assertWorktreeNotExternallyDirty("merge")
	if !errors.Is(err, ErrExternallyDirtyWorktree) {
		t.Fatalf("expected ErrExternallyDirtyWorktree, got %v", err)
	}
	if !strings.Contains(err.Error(), "slot_run_nice_test.go") {
		t.Errorf("refusal must name the offending path, got %v", err)
	}
	if !strings.Contains(err.Error(), "staged") {
		t.Errorf("refusal must say the change is staged, got %v", err)
	}
	if !strings.Contains(err.Error(), "merge checkout, not a working copy") {
		t.Errorf("refusal must carry the remedy, got %v", err)
	}
}

func TestAssertWorktreeNotExternallyDirty_RefusesUncommittedChanges(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	writeFile(t, workDir, "README.md", "# Edited by someone else\n")

	err := e.assertWorktreeNotExternallyDirty("merge")
	if !errors.Is(err, ErrExternallyDirtyWorktree) {
		t.Fatalf("expected ErrExternallyDirtyWorktree, got %v", err)
	}
	if !strings.Contains(err.Error(), "uncommitted") || !strings.Contains(err.Error(), "README.md") {
		t.Errorf("refusal must name an uncommitted path, got %v", err)
	}
}

// An untracked file is never the merge's own residue, but refusing on one
// would stop the queue for good the first time a tool left one behind.
func TestAssertWorktreeNotExternallyDirty_WarnsOnUntrackedFiles(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	writeFile(t, workDir, "scratch.txt", "notes\n")

	if err := e.assertWorktreeNotExternallyDirty("merge"); err != nil {
		t.Fatalf("an untracked file must not block the queue: %v", err)
	}
	out := engineerOutput(t, e)
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "scratch.txt") {
		t.Errorf("untracked files must still be reported, got output %q", out)
	}
}

func TestAssertWorktreeNotExternallyDirty_RefusesDeletedTrackedFiles(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	run(t, workDir, "git", "rm", "README.md")

	err := e.assertWorktreeNotExternallyDirty("merge")
	if !errors.Is(err, ErrExternallyDirtyWorktree) {
		t.Fatalf("expected ErrExternallyDirtyWorktree, got %v", err)
	}
	if !strings.Contains(err.Error(), "README.md") {
		t.Errorf("refusal must name the deleted path, got %v", err)
	}
}

func TestAssertWorktreeNotExternallyDirty_AllowsCleanWorktree(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	if err := e.assertWorktreeNotExternallyDirty("merge"); err != nil {
		t.Fatalf("a clean worktree must not block a merge: %v", err)
	}
}

// A crashed merge leaves its own residue, and the marker is the only thing
// that tells that apart from an external writer. Without this the guard would
// wedge the merge queue on every crash (gt-evk4, gt-cmv class).
func TestAssertWorktreeNotExternallyDirty_AllowsResidueFromInterruptedMerge(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	if err := e.markWorktreeInFlight(); err != nil {
		t.Fatalf("markWorktreeInFlight: %v", err)
	}
	writeFile(t, workDir, "README.md", "# half-finished merge\n")

	if err := e.assertWorktreeNotExternallyDirty("merge"); err != nil {
		t.Fatalf("the refinery's own residue must be restorable, got %v", err)
	}
	if out := engineerOutput(t, e); !strings.Contains(out, "interrupted merge") {
		t.Errorf("restoring residue must be reported, got output %q", out)
	}
}

// A marker left behind by a clean run would let the next external write read
// as refinery residue and be reset away unread, so a clean tree clears it.
func TestAssertWorktreeNotExternallyDirty_ClearsStaleMarkerOnCleanTree(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	if err := e.markWorktreeInFlight(); err != nil {
		t.Fatalf("markWorktreeInFlight: %v", err)
	}
	if err := e.assertWorktreeNotExternallyDirty("merge"); err != nil {
		t.Fatalf("a clean worktree must not block a merge: %v", err)
	}
	if e.worktreeInFlight() {
		t.Fatal("a clean worktree must drop the in-flight marker")
	}

	// With the marker gone, a later external write is refused rather than
	// silently attributed to the refinery.
	writeFile(t, workDir, "README.md", "# someone else\n")
	if err := e.assertWorktreeNotExternallyDirty("merge"); !errors.Is(err, ErrExternallyDirtyWorktree) {
		t.Fatalf("expected a refusal after the stale marker was cleared, got %v", err)
	}
}

func TestClearWorktreeInFlightWhenClean_KeepsMarkerWhileTreeIsDirty(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	if err := e.markWorktreeInFlight(); err != nil {
		t.Fatalf("markWorktreeInFlight: %v", err)
	}
	dirtyStagedFile(t, workDir, "mid-merge.go")

	e.clearWorktreeInFlightWhenClean()
	if !e.worktreeInFlight() {
		t.Fatal("a dirty worktree must keep its marker: it is what attributes the dirt")
	}

	run(t, workDir, "git", "reset", "--hard", "HEAD")
	run(t, workDir, "git", "clean", "-fd")
	e.clearWorktreeInFlightWhenClean()
	if e.worktreeInFlight() {
		t.Fatal("a clean worktree must drop the marker")
	}
}

func TestBeginWorktreeOwnedMerge_ClaimsThenReleasesCleanWorktree(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)

	release, err := e.beginWorktreeOwnedMerge("merge")
	if err != nil {
		t.Fatalf("beginWorktreeOwnedMerge: %v", err)
	}
	if !e.worktreeInFlight() {
		t.Fatal("the merge must be recorded as owning the worktree")
	}

	release()
	if e.worktreeInFlight() {
		t.Fatal("a clean worktree must release the claim")
	}
}

// The refusal has to happen before the merge's first mutation: the merge
// starts by resetting the target to origin, which would otherwise destroy the
// external work without anyone ever seeing it.
func TestDoMerge_RefusesExternallyDirtyWorktreeAndKeepsTheWork(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)
	createFeatureBranch(t, workDir, "polecat/test/gt-nnwy", "feature.txt", "feature\n")

	dirtyStagedFile(t, workDir, "slot_run_nice_test.go")
	writeFile(t, workDir, "scratch.txt", "someone's notes\n")
	headBefore := run(t, workDir, "git", "rev-parse", "HEAD")

	mr := makeMR("test-mr", "polecat/test/gt-nnwy", "main")
	result := e.doMerge(context.Background(), mr)

	if result.Success {
		t.Fatal("a merge must not run over work an external writer left staged")
	}
	if !result.WorktreeExternallyDirty {
		t.Fatalf("result must classify the refusal, got %+v", result)
	}
	if !strings.Contains(result.Error, "slot_run_nice_test.go") {
		t.Errorf("refusal must name the offending path, got %q", result.Error)
	}

	if got := run(t, workDir, "git", "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("the refusal must not move HEAD: was %s, now %s", headBefore, got)
	}
	staged := run(t, workDir, "git", "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "slot_run_nice_test.go") {
		t.Errorf("the refusal must leave the staged work staged, got %q", staged)
	}
	if _, err := os.Stat(filepath.Join(workDir, "scratch.txt")); err != nil {
		t.Errorf("the refusal must leave untracked work in place: %v", err)
	}
	if e.worktreeInFlight() {
		t.Error("a refusal that never touched the tree must not leave an in-flight marker")
	}
}

// A refused merge needs a human, and nothing else in the town sees it: the
// refusal is not a verdict about any MR, so the MR-side notification paths stay
// silent and the blockage goes to the witness.
func TestDoMerge_RefusalEscalatesToTheWitnessOnce(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)
	escalations := []string{}
	e.escalateFn = func(msg string) { escalations = append(escalations, msg) }

	createFeatureBranch(t, workDir, "polecat/test/gt-nnwy", "feature.txt", "feature\n")
	dirtyStagedFile(t, workDir, "slot_run_nice_test.go")
	mr := makeMR("test-mr", "polecat/test/gt-nnwy", "main")

	for i := 0; i < 2; i++ {
		result := e.doMerge(context.Background(), mr)
		if !result.WorktreeExternallyDirty {
			t.Fatalf("expected a classified refusal, got %+v", result)
		}
		e.HandleMRInfoFailure(mr, result)
	}

	if len(escalations) != 1 {
		t.Fatalf("expected exactly one escalation for a blockage that repeats every cycle, got %d: %v", len(escalations), escalations)
	}
	if !strings.Contains(escalations[0], "REFINERY_WORKTREE_DIRTY") || !strings.Contains(escalations[0], workDir) {
		t.Errorf("escalation must name the blockage and the worktree, got %q", escalations[0])
	}
}

func TestDoMerge_LeavesNoMarkerAfterRefusal(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)
	createFeatureBranch(t, workDir, "polecat/test/gt-nnwy", "feature.txt", "feature\n")

	dirtyStagedFile(t, workDir, "slot_run_nice_test.go")
	mr := makeMR("test-mr", "polecat/test/gt-nnwy", "main")
	e.doMerge(context.Background(), mr)

	// A marker here would attribute the *next* refusal — a real external
	// write — to the refinery and let it be reset away.
	if e.worktreeInFlight() {
		t.Fatal("a refused merge must not claim the worktree")
	}
}

// The multi-MR batch stages its rebase stack without going through doMerge,
// so it needs the same refusal (gt-nnwy).
func TestProcessBatch_RefusesExternallyDirtyWorktree(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	e := newTestEngineer(t, workDir, g)
	e.escalateFn = func(string) {} // the refusal escalates; tests must not reach a live witness
	createFeatureBranch(t, workDir, "polecat/test/gt-a", "a.txt", "a\n")
	createFeatureBranch(t, workDir, "polecat/test/gt-b", "b.txt", "b\n")

	dirtyStagedFile(t, workDir, "slot_run_nice_test.go")

	batch := []*MRInfo{
		makeMR("mr-a", "polecat/test/gt-a", "main"),
		makeMR("mr-b", "polecat/test/gt-b", "main"),
	}
	result := e.ProcessBatch(context.Background(), batch, "main", nil)

	if result.Error == nil {
		t.Fatal("a batch must not run over work an external writer left staged")
	}
	if !result.WorktreeExternallyDirty {
		t.Fatalf("result must classify the refusal, got %+v", result)
	}
	if len(result.Merged) != 0 || len(result.Culprits) != 0 || len(result.Conflicts) != 0 {
		t.Errorf("no batch member was examined, so none may be blamed: %+v", result)
	}
	if e.worktreeInFlight() {
		t.Error("a refused batch must not claim the worktree")
	}
}
