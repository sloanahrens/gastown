package refinery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// writeApproveNote writes a matching approve note for head, sized for the
// resume tests in this file: base is head's own merge-base with origin/main,
// so the note's patch-id is exactly the diff editorialPrecondition would
// have required before the original (simulated) push.
func writeApproveNote(t *testing.T, g *gitpkg.Git, mrID, worker, head string) string {
	t.Helper()
	base, err := g.MergeBase("origin/main", head)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	patchID, err := g.PatchID(base, head)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if err := editorial.WriteNote(g, editorial.Note{
		OMVersion:  "1.4.0",
		Rig:        "test-rig",
		MR:         mrID,
		Worker:     worker,
		BaseSHA:    base,
		HeadSHA:    head,
		PatchID:    patchID,
		Score:      0.9,
		Verdict:    "approve",
		Attempt:    1,
		ReviewedAt: time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	return patchID
}

// simulateCrashedMerge performs, by hand, exactly what doMerge's own merge
// step would have done for branch — merge into main and push — and stops
// there, the way a refinery process death right between the push and
// HandleMRInfoSuccess (which never gets to run) would leave things: origin's
// default branch has moved, but nothing else about the MR (its bead, and
// under editorial.required its note on the landed commit) was touched.
func simulateCrashedMerge(t *testing.T, workDir, branch string) (landedCommit string) {
	t.Helper()
	run(t, workDir, "git", "checkout", "main")
	run(t, workDir, "git", "merge", "--no-ff", branch, "-m", "Merge "+branch+" into main")
	landedCommit = run(t, workDir, "git", "rev-parse", "HEAD")
	run(t, workDir, "git", "push", "origin", "main")
	run(t, workDir, "git", "checkout", branch)
	run(t, workDir, "git", "checkout", "main")
	return landedCommit
}

// TestDoMerge_ResumeAfterInterruptedBookkeeping_NoOtherActivitySince covers
// gt-wh66's core failure: a prior pass merged and pushed an MR's commit but
// died before running any bookkeeping at all (no note copy, no MR/bead
// close). doMerge is simply invoked again, as the next refinery cycle would.
// It must recognize the commit as already landed — not re-merge it, and not
// misclassify it as an empty/no-op MR the way the pre-existing empty-merge
// check would when nothing else landed on target in between — and it must
// backfill the om note from the pre-push approve note it can still find by
// patch-id.
func TestDoMerge_ResumeAfterInterruptedBookkeeping_NoOtherActivitySince(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/resume-crash"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)
	patchID := writeApproveNote(t, g, "mr-resume-crash", "polecats/max", head)

	landedCommit := simulateCrashedMerge(t, workDir, branch)
	if _, err := editorial.ReadNote(g, landedCommit); err != gitpkg.ErrNoNote {
		t.Fatalf("expected the landed commit to start with no note (simulating the crash), got err=%v", err)
	}

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	mr := &MRInfo{
		ID:        "mr-resume-crash",
		Branch:    branch,
		Target:    "main",
		Worker:    "polecats/max",
		CommitSHA: head,
	}

	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed to resume an already-landed MR: %s", result.Error)
	}
	if result.MergeCommit != landedCommit {
		t.Fatalf("expected resume to report the already-landed merge commit %s, got %s", landedCommit, result.MergeCommit)
	}

	afterHead := run(t, workDir, "git", "rev-parse", "origin/main")
	if afterHead != landedCommit {
		t.Fatalf("doMerge re-merged an already-landed MR: origin/main moved from %s to %s", landedCommit, afterHead)
	}

	note, err := editorial.ReadNote(g, landedCommit)
	if err != nil {
		t.Fatalf("expected the landed commit to carry a backfilled om note after resume, got: %v", err)
	}
	if note.Verdict != "approve" {
		t.Fatalf("expected an approve note, got verdict %q", note.Verdict)
	}
	if note.PatchID != patchID {
		t.Fatalf("backfilled note patch-id mismatch: got %s, want %s", note.PatchID, patchID)
	}
	if !note.Backfill {
		t.Fatal("expected the backfilled note to be marked Backfill")
	}
}

// TestDoMerge_ResumeAfterInterruptedBookkeeping_FullConvergence covers
// acceptance criterion 4 end-to-end: after the simulated crash, resuming
// through doMerge and then HandleMRInfoSuccess (exactly as the refinery's
// normal success path chains them) must not re-merge, must record the om
// note, and must finish the rest of ordinary bookkeeping — deleting the
// now-merged polecat branch both locally and on the remote.
func TestDoMerge_ResumeAfterInterruptedBookkeeping_FullConvergence(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	installNoPRGH(t)
	run(t, workDir, "git", "remote", "add", "upstream", "https://github.com/example/repo.git")

	branch := "polecat/test/resume-crash-converge"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)
	run(t, workDir, "git", "push", "origin", branch)
	writeApproveNote(t, g, "mr-resume-crash-converge", "polecats/max", head)

	landedCommit := simulateCrashedMerge(t, workDir, branch)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	mr := &MRInfo{
		ID:        "mr-resume-crash-converge",
		Branch:    branch,
		Target:    "main",
		Worker:    "polecats/max",
		CommitSHA: head,
	}

	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed to resume: %s", result.Error)
	}
	if !e.HandleMRInfoSuccess(mr, result) {
		t.Fatal("HandleMRInfoSuccess reported failure completing the resumed merge")
	}

	if afterHead := run(t, workDir, "git", "rev-parse", "origin/main"); afterHead != landedCommit {
		t.Fatalf("origin/main moved during convergence: want %s, got %s", landedCommit, afterHead)
	}
	if _, err := editorial.ReadNote(g, landedCommit); err != nil {
		t.Fatalf("expected the landed commit to carry its om note after convergence, got: %v", err)
	}
	if exists := run(t, workDir, "git", "branch", "--list", branch); exists != "" {
		t.Errorf("expected local branch %s to be deleted after convergence, still present: %q", branch, exists)
	}
	if exists := run(t, workDir, "git", "ls-remote", "--heads", "origin", branch); strings.TrimSpace(exists) != "" {
		t.Errorf("expected remote branch %s to be deleted after convergence, still present: %q", branch, exists)
	}
}

// TestDoMerge_ResumeAfterInterruptedBookkeeping_LaterMRsAlreadyLanded covers
// the realistic incident shape: by the time the next refinery cycle picks
// the crashed MR back up, one or more later MRs have already landed on top
// of it. findMergeCommitFor must still find the exact merge commit that
// brought this MR's own commit onto target, not just the current tip.
func TestDoMerge_ResumeAfterInterruptedBookkeeping_LaterMRsAlreadyLanded(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/resume-crash-2"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)
	patchID := writeApproveNote(t, g, "mr-resume-crash-2", "polecats/max", head)

	landedCommit := simulateCrashedMerge(t, workDir, branch)

	// A second, unrelated MR lands normally afterward — the refinery came
	// back up and kept processing the rest of the queue while this MR's
	// bead was still stuck 'ready'.
	otherBranch := "polecat/test/resume-crash-2-other"
	createFeatureBranch(t, workDir, otherBranch, "other.txt", "other\n")
	writeApproveNote(t, g, "mr-resume-crash-2-other", "polecats/max", run(t, workDir, "git", "rev-parse", otherBranch))
	run(t, workDir, "git", "merge", "--no-ff", otherBranch, "-m", "Merge "+otherBranch+" into main")
	run(t, workDir, "git", "push", "origin", "main")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	mr := &MRInfo{
		ID:        "mr-resume-crash-2",
		Branch:    branch,
		Target:    "main",
		Worker:    "polecats/max",
		CommitSHA: head,
	}

	beforeHead := run(t, workDir, "git", "rev-parse", "origin/main")
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed to resume an already-landed MR behind later work: %s", result.Error)
	}
	if result.MergeCommit != landedCommit {
		t.Fatalf("expected resume to identify this MR's own merge commit %s, got %s", landedCommit, result.MergeCommit)
	}
	if afterHead := run(t, workDir, "git", "rev-parse", "origin/main"); afterHead != beforeHead {
		t.Fatalf("doMerge moved origin/main during resume: before=%s after=%s", beforeHead, afterHead)
	}

	note, err := editorial.ReadNote(g, landedCommit)
	if err != nil {
		t.Fatalf("expected the landed commit to carry a backfilled om note, got: %v", err)
	}
	if note.PatchID != patchID {
		t.Fatalf("backfilled note patch-id mismatch: got %s, want %s", note.PatchID, patchID)
	}
}

// TestResumeLandedMerge_ContentPreservedRebase_BackfillsNoteByPatchID covers
// the case mergeAlreadyLanded's cherry fallback recognizes but findMergeCommitFor
// cannot resolve: a rebase changed the submitted commit's SHA before it
// landed, so that original SHA is not literally on target — only its
// content is, by patch-id. resumeLandedMerge is exercised directly (rather
// than through doMerge, whose stricter submittedBranchHead check requires
// the live branch to sit exactly at the recorded commit_sha, which a
// same-session rebase would already have updated) with mergeRef set to that
// stale, unreachable original SHA — reproducing a bead that still names the
// pre-rebase head.
//
// The MR was reviewed, so its note exists (on the pre-rebase head). Resume
// must resolve the commit the content actually landed as, by patch-id, and
// backfill the note onto that commit — not guess a sha, and not falsely
// escalate an MR that landed cleanly (the property this test has always
// guarded, now on the path that has to prove it).
func TestResumeLandedMerge_ContentPreservedRebase_BackfillsNoteByPatchID(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	branch := "polecat/test/resume-crash-rebase"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	submitted := run(t, workDir, "git", "rev-parse", branch)
	patchID := writeApproveNote(t, g, "mr-resume-crash-rebase", "polecats/max", submitted)

	// Another MR lands on target first, moving it ahead of feature's base.
	run(t, workDir, "git", "checkout", "main")
	writeFile(t, workDir, "other.txt", "other\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "other: unrelated MR landed first")
	run(t, workDir, "git", "push", "origin", "main")

	// Rebase feature onto the moved target — same patch, new SHA — and land it.
	run(t, workDir, "git", "checkout", branch)
	run(t, workDir, "git", "rebase", "main")
	rebased := run(t, workDir, "git", "rev-parse", branch)
	if rebased == submitted {
		t.Fatal("rebase did not change the commit SHA")
	}
	run(t, workDir, "git", "checkout", "main")
	run(t, workDir, "git", "merge", "--ff-only", branch)
	run(t, workDir, "git", "push", "origin", "main")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	// mergeRef is the stale, pre-rebase SHA a bead written before the rebase
	// would still carry: not itself reachable from target, though its
	// content is.
	if reachable, _ := g.IsAncestor(submitted, "origin/main"); reachable {
		t.Fatal("test setup: the stale SHA should not be a literal ancestor of target")
	}
	if _, err := editorial.ReadNote(g, rebased); err != gitpkg.ErrNoNote {
		t.Fatalf("test setup: the landed commit should start with no note of its own, got err=%v", err)
	}

	before := run(t, workDir, "git", "rev-parse", "origin/main")
	result := e.resumeLandedMerge(&MRInfo{ID: "mr-resume-crash-rebase"}, "main", submitted)
	if !result.Success {
		t.Fatalf("resumeLandedMerge failed for a reviewed content-preserved rebase landing: %s", result.Error)
	}
	if result.MergeCommit != rebased {
		t.Fatalf("expected resume to report the commit the content landed as %s, got %s", rebased, result.MergeCommit)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("resumeLandedMerge moved origin/main: before=%s after=%s", before, after)
	}

	note, err := editorial.ReadNote(g, rebased)
	if err != nil {
		t.Fatalf("expected the landed commit to carry the backfilled om note, got: %v", err)
	}
	if note.Verdict != "approve" || note.PatchID != patchID {
		t.Fatalf("backfilled note = verdict %q patch-id %s, want approve/%s", note.Verdict, note.PatchID, patchID)
	}
	if !note.Backfill || note.RekeyedFrom != submitted {
		t.Fatalf("expected a backfill rekeyed from the reviewed sha %s, got backfill=%v rekeyed_from=%q", submitted, note.Backfill, note.RekeyedFrom)
	}

	if bd := readLog(t, bdLog); strings.Contains(bd, "record_failed") {
		t.Fatalf("expected no record_failed escalation for content that genuinely landed and was reviewed, bd log:\n%s", bd)
	}
	if gtCalls := readLog(t, gtLog); strings.Contains(gtCalls, "nudge") {
		t.Fatalf("expected no escalation for a reviewed landing that resumed cleanly, gt log:\n%s", gtCalls)
	}
}

// simulateCherryPickedLanding lands branch's commit on main as a cherry-pick
// — a new sha carrying the same patch, the shape a rebase or cherry-pick
// queue produces — and pushes. Unlike simulateCrashedMerge it leaves the
// branch itself untouched, so an MR bead still naming that commit_sha passes
// doMerge's submittedBranchHead check and the resume path is reached the way
// a real queue cycle reaches it.
func simulateCherryPickedLanding(t *testing.T, workDir, branch string) (landedCommit string) {
	t.Helper()
	head := run(t, workDir, "git", "rev-parse", branch)
	run(t, workDir, "git", "checkout", "main")
	run(t, workDir, "git", "cherry-pick", head)
	landedCommit = run(t, workDir, "git", "rev-parse", "HEAD")
	run(t, workDir, "git", "push", "origin", "main")
	return landedCommit
}

// atCommit fails unless branch still resolves to commit — the state a queue
// cycle leaves a polecat branch it has not merged, and what lets doMerge's
// submittedBranchHead check pass so the resume path is reached the way a real
// cycle reaches it.
func atCommit(t *testing.T, workDir, branch, commit string) {
	t.Helper()
	if got := run(t, workDir, "git", "rev-parse", branch); got != commit {
		t.Fatalf("test setup: branch %s is at %s, expected the recorded commit_sha %s", branch, got, commit)
	}
}

// TestDoMerge_ResumeAfterInterruptedBookkeeping_RenamedLanding_NoNote_Refuses
// is gt-9t0p's core case, driven end to end through doMerge: the submitted
// commit's content is on target under a sha a cherry-pick rewrote, and no om
// note covers it. The MR is NOT completed — no merge_commit is reported, no
// bookkeeping runs — and the missing proof is recorded and escalated.
//
// Before this fix the rebase/cherry branch of resumeLandedMerge printed a
// hint and returned success, so the MR closed and its source issue with it,
// on content nobody had a verdict for: the silent route past
// merge_queue.editorial.required that gt-wh66's criterion 5 forbids.
func TestDoMerge_ResumeAfterInterruptedBookkeeping_RenamedLanding_NoNote_Refuses(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	branch := "polecat/test/resume-renamed-nonote"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)

	// Another MR lands first, then this one's patch is cherry-picked on top of
	// it — a new sha carrying the same content, with the branch itself left
	// where the MR bead says it is.
	run(t, workDir, "git", "checkout", "main")
	writeFile(t, workDir, "other.txt", "other\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "other: unrelated MR landed first")
	run(t, workDir, "git", "push", "origin", "main")
	landed := simulateCherryPickedLanding(t, workDir, branch)
	atCommit(t, workDir, branch, head)

	if reachable, err := g.IsAncestor(head, "origin/main"); err != nil || reachable {
		t.Fatalf("test setup: the submitted sha must not be a literal ancestor of target (err=%v)", err)
	}
	if !g.CommitLandedOnTarget("origin", "main", head) {
		t.Fatal("test setup: the cherry-picked content must read as landed")
	}

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	mr := &MRInfo{
		ID:        "mr-resume-renamed-nonote",
		Branch:    branch,
		Target:    "main",
		Worker:    "polecats/max",
		CommitSHA: head,
	}

	before := run(t, workDir, "git", "rev-parse", "origin/main")
	result := e.doMerge(context.Background(), mr)
	if result.Success {
		t.Fatalf("doMerge completed an unproven renamed landing (merge_commit=%s): %s", result.MergeCommit, result.Error)
	}
	if !result.EditorialRefused {
		t.Fatalf("expected an editorial refusal, got %+v", result)
	}
	if result.EditorialReason != editorial.ReasonMissing {
		t.Fatalf("expected reason %q, got %q", editorial.ReasonMissing, result.EditorialReason)
	}
	if result.MergeCommit != "" {
		t.Fatalf("a refused landing must not report a merge commit, got %s", result.MergeCommit)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("doMerge moved origin/main while refusing: before=%s after=%s", before, after)
	}
	if _, err := editorial.ReadNote(g, landed); err != gitpkg.ErrNoNote {
		t.Fatalf("a refusal must not invent a note on the landed commit, got err=%v", err)
	}

	bd := readLog(t, bdLog)
	if !strings.Contains(bd, "failure_class:precondition") {
		t.Fatalf("expected a precondition failure receipt for the unproven landing, bd log:\n%s", bd)
	}
	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "test-rig/witness") || !strings.Contains(gtCalls, "EDITORIAL_RESUME_UNPROVEN") {
		t.Fatalf("expected the witness to be nudged about the unproven landing, gt log:\n%s", gtCalls)
	}
	if !strings.Contains(gtCalls, "rekey-note mr-resume-renamed-nonote --landed "+landed) {
		t.Fatalf("expected the escalation to name the remedy and the landed sha, gt log:\n%s", gtCalls)
	}
}

// TestDoMerge_ResumeAfterInterruptedBookkeeping_RenamedLanding_NoteBackfilled
// is the other half of that case: the same renamed landing, but the MR was
// reviewed, so its approve note exists keyed to the pre-cherry-pick sha.
// Resume resolves the landed commit by patch-id, backfills the note onto it,
// and completes — a reviewed MR must not wedge in the queue for a sha that a
// rebase rewrote (gt-9t0p: keyed by patch-id when the SHA changed).
func TestDoMerge_ResumeAfterInterruptedBookkeeping_RenamedLanding_NoteBackfilled(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/resume-renamed-note"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)
	patchID := writeApproveNote(t, g, "mr-resume-renamed-note", "polecats/max", head)

	run(t, workDir, "git", "checkout", "main")
	writeFile(t, workDir, "other.txt", "other\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "other: unrelated MR landed first")
	run(t, workDir, "git", "push", "origin", "main")
	landed := simulateCherryPickedLanding(t, workDir, branch)
	atCommit(t, workDir, branch, head)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	mr := &MRInfo{
		ID:        "mr-resume-renamed-note",
		Branch:    branch,
		Target:    "main",
		Worker:    "polecats/max",
		CommitSHA: head,
	}

	before := run(t, workDir, "git", "rev-parse", "origin/main")
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge refused a reviewed renamed landing: %s", result.Error)
	}
	if result.MergeCommit != landed {
		t.Fatalf("expected the landed commit %s to be reported, got %s", landed, result.MergeCommit)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("doMerge moved origin/main during resume: before=%s after=%s", before, after)
	}

	note, err := editorial.ReadNote(g, landed)
	if err != nil {
		t.Fatalf("expected the landed commit to carry its backfilled note, got: %v", err)
	}
	if note.Verdict != "approve" || note.PatchID != patchID || !note.Backfill {
		t.Fatalf("backfilled note = verdict %q patch-id %s backfill=%v, want approve/%s/true", note.Verdict, note.PatchID, note.Backfill, patchID)
	}
	if note.RekeyedFrom != head {
		t.Fatalf("expected the note to be rekeyed from the reviewed sha %s, got %q", head, note.RekeyedFrom)
	}
}

// TestDoMerge_ResumeAfterInterruptedBookkeeping_EditorialNotRequired_NoOp
// covers a rig that has not opted into merge_queue.editorial.required:
// resume still completes (no re-merge), but nothing touches refs/notes/om.
func TestDoMerge_ResumeAfterInterruptedBookkeeping_EditorialNotRequired_NoOp(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/resume-crash-noeditorial"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)
	landedCommit := simulateCrashedMerge(t, workDir, branch)

	e := newTestEngineer(t, workDir, g)
	// e.config.Editorial left nil: not required.

	mr := &MRInfo{
		ID:        "mr-resume-crash-noeditorial",
		Branch:    branch,
		Target:    "main",
		Worker:    "polecats/max",
		CommitSHA: head,
	}

	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed to resume an already-landed MR: %s", result.Error)
	}
	if result.MergeCommit != landedCommit {
		t.Fatalf("expected resume to report the already-landed commit %s, got %s", landedCommit, result.MergeCommit)
	}
	if _, err := editorial.ReadNote(g, landedCommit); err != gitpkg.ErrNoNote {
		t.Fatalf("expected no note written when editorial is not required, got err=%v", err)
	}
}

// TestDoMerge_ResumeAfterInterruptedBookkeeping_NoteUnrecoverable_EscalatesButCompletes
// covers the case reconcileLandedEditorialNote cannot repair: no pre-push
// approve note exists for this MR at all (the note-writing step of the
// original pass never ran, or the note was lost). Bookkeeping must not hang
// forever on a commit that already landed — it completes anyway — but the
// missing proof must be recorded and escalated, not silently dropped
// (acceptance criterion 5).
func TestDoMerge_ResumeAfterInterruptedBookkeeping_NoteUnrecoverable_EscalatesButCompletes(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	branch := "polecat/test/resume-crash-nonote"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)
	// Deliberately no writeApproveNote: nothing to backfill from.
	landedCommit := simulateCrashedMerge(t, workDir, branch)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	mr := &MRInfo{
		ID:        "mr-resume-crash-nonote",
		Branch:    branch,
		Target:    "main",
		Worker:    "polecats/max",
		CommitSHA: head,
	}

	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge should complete resume even without a recoverable note: %s", result.Error)
	}
	if result.MergeCommit != landedCommit {
		t.Fatalf("expected the resume to still identify the landed commit %s, got %s", landedCommit, result.MergeCommit)
	}
	if _, err := editorial.ReadNote(g, landedCommit); err != gitpkg.ErrNoNote {
		t.Fatalf("expected still no note on the landed commit (nothing to backfill from), got err=%v", err)
	}

	bd := readLog(t, bdLog)
	if !strings.Contains(bd, "record_failed") {
		t.Fatalf("expected a record_failed failure receipt, bd log:\n%s", bd)
	}
	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "nudge") || !strings.Contains(gtCalls, "test-rig/witness") {
		t.Fatalf("expected the witness to be nudged about the unrecoverable note, gt log:\n%s", gtCalls)
	}
}
