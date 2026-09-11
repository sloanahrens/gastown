package refinery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// TestDoMerge_SecondSequentialMerge_OntoPriorMergeCommit_NoteReachesLandedCommit
// reproduces gt-4n7l: MR1 lands via doMerge producing a merge commit M1 on
// main. MR2's branch is rebased onto M1 (so M1 becomes the second merge's
// first parent, exactly the "main-first-parent" shape reported: d47a428's
// parents were [b3d0b80, 991b187], where b3d0b80 was itself a merge commit
// that had already landed and been pushed). MR2's EditorialReviewedHead is
// the (single-parent) rebased branch tip, not the eventual merge commit —
// same as production, where "gt mq review" reviewed the branch tip before
// the refinery merged it. The note must end up on MR2's landed merge commit.
func TestDoMerge_SecondSequentialMerge_OntoPriorMergeCommit_NoteReachesLandedCommit(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	// --- MR1: lands normally, producing merge commit M1 on main ---
	branch1 := "polecat/test/mr1"
	createFeatureBranch(t, workDir, branch1, "file1.txt", "one\n")
	head1 := run(t, workDir, "git", "rev-parse", branch1)
	base1, err := g.MergeBase("origin/main", head1)
	if err != nil {
		t.Fatalf("MergeBase MR1: %v", err)
	}
	patchID1, err := g.PatchID(base1, head1)
	if err != nil {
		t.Fatalf("PatchID MR1: %v", err)
	}
	if err := editorial.WriteNote(g, editorial.Note{
		OMVersion:  "1.4.0",
		Rig:        "test-rig",
		MR:         "mr1",
		Worker:     "polecats/basalt",
		BaseSHA:    base1,
		HeadSHA:    head1,
		PatchID:    patchID1,
		Score:      0.9,
		Verdict:    "approve",
		Attempt:    1,
		ReviewedAt: time.Date(2026, 9, 11, 8, 4, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteNote MR1: %v", err)
	}

	mr1 := &MRInfo{
		ID:                    "mr1",
		Branch:                branch1,
		Target:                "main",
		Worker:                "polecats/basalt",
		EditorialReviewedHead: head1,
	}
	result1 := e.doMerge(context.Background(), mr1)
	if !result1.Success {
		t.Fatalf("doMerge MR1 failed: %s", result1.Error)
	}
	m1 := run(t, workDir, "git", "rev-parse", "origin/main")
	if m1 == head1 {
		t.Fatal("expected MR1 to land as a distinct merge commit")
	}
	if _, err := editorial.ReadNote(g, m1); err != nil {
		t.Fatalf("MR1's note did not reach its own landed merge commit %s: %v", m1, err)
	}

	// --- MR2: branch rebased onto M1 (so M1 is main-first-parent of MR2's
	// eventual merge commit), reviewed on its own (single-parent) tip ---
	branch2 := "polecat/test/mr2"
	createFeatureBranch(t, workDir, branch2, "file2.txt", "two\n")
	// Rebase branch2 onto the now-updated main (tip M1) — mirrors a polecat
	// rebasing onto latest main before submitting, same shape as 991b187
	// having b3d0b80 as its sole parent.
	run(t, workDir, "git", "checkout", branch2)
	run(t, workDir, "git", "rebase", "main")
	head2 := run(t, workDir, "git", "rev-parse", branch2)
	run(t, workDir, "git", "checkout", "main")

	parents2 := run(t, workDir, "git", "log", "-1", "--format=%P", head2)
	if strings.Contains(parents2, " ") {
		t.Fatalf("expected head2 to have a single parent after rebase, got parents=%q", parents2)
	}
	if parents2 != m1 {
		t.Fatalf("expected head2's sole parent to be M1 (%s), got %s", m1, parents2)
	}

	base2, err := g.MergeBase("origin/main", head2)
	if err != nil {
		t.Fatalf("MergeBase MR2: %v", err)
	}
	patchID2, err := g.PatchID(base2, head2)
	if err != nil {
		t.Fatalf("PatchID MR2: %v", err)
	}
	if err := editorial.WriteNote(g, editorial.Note{
		OMVersion:  "1.4.0",
		Rig:        "test-rig",
		MR:         "mr2",
		Worker:     "polecats/emerald",
		BaseSHA:    base2,
		HeadSHA:    head2,
		PatchID:    patchID2,
		Score:      0.85,
		Verdict:    "approve",
		Attempt:    1,
		ReviewedAt: time.Date(2026, 9, 11, 8, 51, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteNote MR2: %v", err)
	}

	mr2 := &MRInfo{
		ID:                    "mr2",
		Branch:                branch2,
		Target:                "main",
		Worker:                "polecats/emerald",
		EditorialReviewedHead: head2,
	}
	result2 := e.doMerge(context.Background(), mr2)
	if !result2.Success {
		t.Fatalf("doMerge MR2 failed: %s", result2.Error)
	}

	m2 := run(t, workDir, "git", "rev-parse", "origin/main")
	if m2 == head2 {
		t.Fatal("expected MR2 to land as a distinct merge commit")
	}
	parentsM2 := run(t, workDir, "git", "log", "-1", "--format=%P", m2)
	if parentsM2 != m1+" "+head2 {
		t.Fatalf("expected M2's parents to be [M1, head2] = [%s, %s], got %q", m1, head2, parentsM2)
	}

	// THE ACCEPTANCE CRITERION FROM gt-4n7l: the landed merge commit must
	// carry the note (keyed to the reviewed patch-id), same as the second
	// parent.
	if _, err := editorial.ReadNote(g, head2); err != nil {
		t.Fatalf("expected note still present on submitted head2 %s: %v", head2, err)
	}
	noteOnLanded, err := editorial.ReadNote(g, m2)
	if err != nil {
		out := e.output.(interface{ String() string }).String()
		t.Fatalf("gt-4n7l regression reproduced: no note on landed merge commit %s: %v\nengineer output:\n%s", m2, err, out)
	}
	if noteOnLanded.PatchID != patchID2 {
		t.Fatalf("note on landed commit has wrong patch-id: got %s, want %s", noteOnLanded.PatchID, patchID2)
	}
}
