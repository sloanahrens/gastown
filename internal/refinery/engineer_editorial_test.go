package refinery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/rig"
)

// fakeBDAndGt installs bd and gt stand-ins on PATH that log their
// invocations, so editorial-precondition tests can assert a failure
// receipt was recorded and the witness was nudged without touching real
// beads/gt state. Mirrors internal/refinery/editorial's own fakeBD helper.
func fakeBDAndGt(t *testing.T) (bdLog, gtLog string) {
	t.Helper()
	binDir := t.TempDir()
	bdLog = filepath.Join(t.TempDir(), "bd-args.log")
	gtLog = filepath.Join(t.TempDir(), "gt-args.log")

	bdScript := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"$BD_ARGS_LOG\"\n" +
		"case \"$1\" in\n" +
		"  create) printf '{\\\"id\\\":\\\"gt-test-failure-receipt\\\"}\\n' ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdScript), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	gtScript := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"$GT_ARGS_LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_ARGS_LOG", bdLog)
	t.Setenv("GT_ARGS_LOG", gtLog)
	return bdLog, gtLog
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read log %s: %v", path, err)
	}
	return string(data)
}

// TestDoMerge_EditorialRequired_NoNote_RefusesPush is the Task 6 acceptance
// criterion: doMerge with skipGates=true and no note refuses to push,
// records a precondition failure receipt, and escalates to the witness.
// skipGates must not bypass the editorial precondition.
func TestDoMerge_EditorialRequired_NoNote_RefusesPush(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	branch := "polecat/test/editorial-missing"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	mr := &MRInfo{
		ID:     "mr-editorial-missing",
		Branch: branch,
		Target: "main",
		Worker: "polecats/max",
	}
	result := e.doMerge(context.Background(), mr, true) // skipGates=true

	if result.Success {
		t.Fatalf("doMerge succeeded despite missing editorial note: %+v", result)
	}

	out := e.output.(interface{ String() string }).String()
	wantLog := "[Engineer] editorial precondition failed for mr-editorial-missing: missing"
	if !strings.Contains(out, wantLog) {
		t.Fatalf("output missing precondition log line %q, got:\n%s", wantLog, out)
	}

	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("origin/main advanced despite refused push: before=%s after=%s", before, after)
	}

	bd := readLog(t, bdLog)
	if !strings.Contains(bd, "failure_class:precondition") {
		t.Fatalf("failure receipt missing failure_class:precondition, bd log:\n%s", bd)
	}

	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "nudge") || !strings.Contains(gtCalls, "test-rig/witness") {
		t.Fatalf("witness was not nudged, gt log:\n%s", gtCalls)
	}
}

// TestDoMerge_EditorialRequired_ApproveMatchingNote_PushesAndPublishesNote
// covers the ok path: an approve note whose patch-id matches lets the push
// through, and the notes ref is pushed alongside the target branch.
func TestDoMerge_EditorialRequired_ApproveMatchingNote_PushesAndPublishesNote(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/editorial-approve"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)
	// gt-sda9: doMerge asserts the declared head is reachable from origin's
	// tip before gating; push the branch so that holds.
	run(t, workDir, "git", "push", "-u", "origin", branch)

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
		MR:         "mr-editorial-approve",
		Worker:     "polecats/max",
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

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	mr := &MRInfo{
		ID:                    "mr-editorial-approve",
		Branch:                branch,
		Target:                "main",
		Worker:                "polecats/max",
		EditorialReviewedHead: head,
	}
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed despite matching approve note: %s", result.Error)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after == before {
		t.Fatal("origin/main did not advance despite successful push")
	}

	notesOnRemote := run(t, workDir, "git", "ls-remote", "origin", "refs/notes/om")
	if strings.TrimSpace(notesOnRemote) == "" {
		t.Fatal("expected refs/notes/om to be pushed to origin alongside the merge")
	}
}

// TestDoMerge_EditorialRequired_TargetMovedMaterially_RefusesPush is gt-6bsp's
// acceptance case: main advanced past the reviewed target tip, on the same
// file the MR's own diff touches, before this MR reached the push. The MR's
// own diff is unchanged (patch-id still matches — the existing check this
// test would otherwise pass), but the combined tree that would actually land
// was never reviewed as a tree, so the push must refuse rather than land it
// on an unreviewed note's authority.
func TestDoMerge_EditorialRequired_TargetMovedMaterially_RefusesPush(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	// feature.txt exists on main before the branch is cut, with room for the
	// MR and main's own later advance to edit different lines — a clean
	// 3-way merge, so the refusal below is the drift check and not an
	// ordinary merge conflict.
	writeFile(t, workDir, "feature.txt", "line1\nline2\nline3\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "chore: add feature.txt")
	run(t, workDir, "git", "push", "origin", "main")

	branch := "polecat/test/editorial-drift"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	writeFile(t, workDir, "feature.txt", "line1 EDITED\nline2\nline3\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: edit line1")
	run(t, workDir, "git", "checkout", "main")
	head := run(t, workDir, "git", "rev-parse", branch)

	base, err := g.MergeBase("origin/main", head)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	patchID, err := g.PatchID(base, head)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if err := editorial.WriteNote(g, editorial.Note{
		OMVersion:         "1.4.0",
		Rig:               "test-rig",
		MR:                "mr-editorial-drift",
		Worker:            "polecats/max",
		BaseSHA:           base,
		ReviewedTargetTip: base, // target had not moved yet at review time
		HeadSHA:           head,
		PatchID:           patchID,
		Score:             0.9,
		Verdict:           "approve",
		Attempt:           1,
		ReviewedAt:        time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}

	// main advances, on the same file the MR touches but a different line
	// (clean 3-way merge) — the material case.
	writeFile(t, workDir, "feature.txt", "line1\nline2\nline3 EDITED\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "chore: edit line3 on main")
	run(t, workDir, "git", "push", "origin", "main")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	mr := &MRInfo{
		ID:                    "mr-editorial-drift",
		Branch:                branch,
		Target:                "main",
		Worker:                "polecats/max",
		EditorialReviewedHead: head,
	}
	result := e.doMerge(context.Background(), mr, true) // skipGates=true

	if result.Success {
		t.Fatalf("doMerge succeeded despite material target drift: %+v", result)
	}
	if !result.EditorialRefused {
		t.Fatalf("refusal not classified as editorial: %+v", result)
	}
	if result.EditorialReason != editorial.ReasonTargetDriftMaterial {
		t.Fatalf("EditorialReason = %q, want %q", result.EditorialReason, editorial.ReasonTargetDriftMaterial)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("origin/main advanced despite refused push: before=%s after=%s", before, after)
	}

	if bd := readLog(t, bdLog); !strings.Contains(bd, "failure_class:precondition") {
		t.Fatalf("failure receipt missing failure_class:precondition, bd log:\n%s", bd)
	}
	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "nudge") || !strings.Contains(gtCalls, "test-rig/witness") {
		t.Fatalf("witness was not nudged, gt log:\n%s", gtCalls)
	}
}

// TestDoMerge_EditorialRequired_TargetMovedDisjointFiles_PushesAnyway is the
// companion to the material-drift test above: main moved since review, but
// on a file the MR's own diff never touches, so the two changes cannot
// interact. The push proceeds without a second review — this is the common
// case the drift check must stay cheap for.
func TestDoMerge_EditorialRequired_TargetMovedDisjointFiles_PushesAnyway(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/editorial-drift-disjoint"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)

	base, err := g.MergeBase("origin/main", head)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	patchID, err := g.PatchID(base, head)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if err := editorial.WriteNote(g, editorial.Note{
		OMVersion:         "1.4.0",
		Rig:               "test-rig",
		MR:                "mr-editorial-drift-disjoint",
		Worker:            "polecats/max",
		BaseSHA:           base,
		ReviewedTargetTip: base,
		HeadSHA:           head,
		PatchID:           patchID,
		Score:             0.9,
		Verdict:           "approve",
		Attempt:           1,
		ReviewedAt:        time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}

	// main advances, on a file the MR never touches — the disjoint case.
	writeFile(t, workDir, "unrelated.txt", "unrelated main change\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "chore: edit unrelated.txt on main")
	run(t, workDir, "git", "push", "origin", "main")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	mr := &MRInfo{
		ID:                    "mr-editorial-drift-disjoint",
		Branch:                branch,
		Target:                "main",
		Worker:                "polecats/max",
		EditorialReviewedHead: head,
	}
	result := e.doMerge(context.Background(), mr, true) // skipGates=true

	if !result.Success {
		t.Fatalf("doMerge failed despite disjoint target drift: %s", result.Error)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after == before {
		t.Fatal("origin/main did not advance despite successful push")
	}
}

// TestDoMerge_EditorialRequired_ReviewedHeadDiffersFromLandedCommit_NoteCopied
// covers the copy-on-differing-head path: the reviewed head (from a rebase
// that left the diff unchanged) differs from both the submitted branch tip
// and the actual landed commit (doMerge always lands a new MergeNoFF
// commit), yet the matching patch-id lets the push through, and the note
// ends up readable at the commit that actually landed on origin/main —
// not at the submitted branch tip, which never carries it.
func TestDoMerge_EditorialRequired_ReviewedHeadDiffersFromLandedCommit_NoteCopied(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/editorial-rebase"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	reviewedHead := run(t, workDir, "git", "rev-parse", branch)

	base, err := g.MergeBase("origin/main", reviewedHead)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	patchID, err := g.PatchID(base, reviewedHead)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if err := editorial.WriteNote(g, editorial.Note{
		OMVersion:  "1.4.0",
		Rig:        "test-rig",
		MR:         "mr-editorial-rebase",
		Worker:     "polecats/max",
		BaseSHA:    base,
		HeadSHA:    reviewedHead,
		PatchID:    patchID,
		Score:      0.9,
		Verdict:    "approve",
		Attempt:    1,
		ReviewedAt: time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}

	// Simulate a rebase that leaves the diff identical but changes the
	// commit identity (a new patch-id-preserving SHA) — amending the
	// committer date is enough: `git patch-id` hashes the diff content
	// only, never commit metadata.
	run(t, workDir, "git", "checkout", branch)
	run(t, workDir, "git", "commit", "--amend", "--no-edit", "--date=2026-09-11T00:00:00")
	rebasedHead := run(t, workDir, "git", "rev-parse", branch)
	if rebasedHead == reviewedHead {
		t.Fatal("amend did not change the commit SHA")
	}
	// gt-sda9: doMerge asserts the branch tip is reachable from origin's tip
	// before gating; the amend moved the local tip, so push it.
	run(t, workDir, "git", "push", "--force", "origin", branch)
	run(t, workDir, "git", "checkout", "main")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	mr := &MRInfo{
		ID:                    "mr-editorial-rebase",
		Branch:                branch,
		Target:                "main",
		Worker:                "polecats/max",
		EditorialReviewedHead: reviewedHead,
	}
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed despite matching patch-id after rebase: %s", result.Error)
	}

	landedCommit := run(t, workDir, "git", "rev-parse", "origin/main")
	if landedCommit == rebasedHead {
		t.Fatalf("landed commit %s equals submitted branch tip — expected a new merge commit", landedCommit)
	}

	got, err := editorial.ReadNote(g, landedCommit)
	if err != nil {
		t.Fatalf("note not readable at landed commit %s: %v", landedCommit, err)
	}
	if got.PatchID != patchID {
		t.Fatalf("copied note patch-id mismatch: got %s, want %s", got.PatchID, patchID)
	}

	if _, err := editorial.ReadNote(g, rebasedHead); err == nil {
		t.Fatal("expected no note on the submitted branch tip — only the landed commit should carry it")
	}
}

// TestCopyEditorialNotes_CopyFails_RecordsRecordFailedAndEscalates covers
// the fail-closed gap fixed by gt-qvxf: a non-ff merge (the landed commit
// differs from the reviewed head, same as an ordinary MergeNoFF) where the
// source note is gone by the time the post-push copy runs — mirroring a
// concurrent gt mq review clobbering refs/notes/om between the push
// precondition's read and this call, the class of race that produced
// gt-hpce's landed-but-unproven merge commit 097ab8a — must not silently
// warn and move on. It must record a record_failed failure receipt per MR
// and escalate to the witness, since the code has already landed and the
// push itself can no longer be refused.
func TestCopyEditorialNotes_CopyFails_RecordsRecordFailedAndEscalates(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	branch := "polecat/test/editorial-race"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	reviewedHead := run(t, workDir, "git", "rev-parse", branch)

	base, err := g.MergeBase("origin/main", reviewedHead)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	patchID, err := g.PatchID(base, reviewedHead)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	note := editorial.Note{
		OMVersion:  "1.4.0",
		Rig:        "test-rig",
		MR:         "mr-editorial-race",
		Worker:     "polecats/max",
		BaseSHA:    base,
		HeadSHA:    reviewedHead,
		PatchID:    patchID,
		Score:      0.9,
		Verdict:    "approve",
		Attempt:    1,
		ReviewedAt: time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}
	if err := editorial.WriteNote(g, note); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}

	// Produce a landed commit distinct from reviewedHead — an ordinary
	// non-ff merge commit, the same shape doMerge's MergeNoFF always makes.
	run(t, workDir, "git", "checkout", "main")
	run(t, workDir, "git", "merge", "--no-ff", "-m", "merge for test", branch)
	landedCommit := run(t, workDir, "git", "rev-parse", "HEAD")
	if landedCommit == reviewedHead {
		t.Fatal("expected a distinct merge commit, got a fast-forward")
	}

	// Simulate the note vanishing from refs/notes/om between the push
	// precondition's read (which already found and verified it) and this
	// copy attempt.
	run(t, workDir, "git", "notes", "--ref", editorial.NotesRef, "remove", reviewedHead)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	mr := &MRInfo{ID: "mr-editorial-race", Worker: "polecats/max"}
	e.copyEditorialNotes("[Engineer]", []editorial.LandedMR{{
		MRID:         mr.ID,
		ReviewedHead: reviewedHead,
		LandedCommit: landedCommit,
	}}, []editorial.Note{note}, []*MRInfo{mr})

	out := e.output.(interface{ String() string }).String()
	if !strings.Contains(out, "EDITORIAL_RECORD_FAILED") {
		t.Fatalf("expected EDITORIAL_RECORD_FAILED in output, got:\n%s", out)
	}

	bd := readLog(t, bdLog)
	if !strings.Contains(bd, "failure_class:record_failed") {
		t.Fatalf("failure receipt missing failure_class:record_failed, bd log:\n%s", bd)
	}
	if !strings.Contains(bd, "mr:mr-editorial-race") {
		t.Fatalf("failure receipt missing mr:mr-editorial-race, bd log:\n%s", bd)
	}

	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "nudge") || !strings.Contains(gtCalls, "test-rig/witness") {
		t.Fatalf("witness was not nudged, gt log:\n%s", gtCalls)
	}

	if _, err := editorial.ReadNote(g, landedCommit); err == nil {
		t.Fatal("expected no note on the landed commit — the copy failed, so nothing should be there")
	}
}

// approveNoteFor writes an approve note covering base..head for mrID, the
// shape gt mq review leaves behind for a candidate it approved.
func approveNoteFor(t *testing.T, g *gitpkg.Git, mrID, worker, base, head, patchID string) {
	t.Helper()
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
		t.Fatalf("WriteNote for %s: %v", mrID, err)
	}
}

// TestDoMergePR_EditorialRequired_NoNote_RefusesMerge closes the first
// om-gate T6 precondition gap: merge_strategy=pr used to merge through the
// VCS provider with no approve-note/patch-id check at all, because doMerge
// dispatches to doMergePR before the block that runs the precondition on the
// local-merge path. The refusal must be classified (EditorialRefused +
// EditorialReason) so the caller routes it as a queued gate verdict rather
// than as a build failure.
func TestDoMergePR_EditorialRequired_NoNote_RefusesMerge(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	branch := "polecat/test/editorial-pr-missing"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}
	e.prProvider = &recordingPRProvider{mergeFunc: func(method string) (string, error) {
		t.Fatalf("MergePR called with method %q despite a missing editorial note", method)
		return "", nil
	}}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	mr := &MRInfo{
		ID:        "mr-editorial-pr-missing",
		Branch:    branch,
		Target:    "main",
		Worker:    "polecats/max",
		CommitSHA: head,
	}
	result := e.doMergePR(context.Background(), mr)

	if result.Success {
		t.Fatalf("doMergePR succeeded despite missing editorial note: %+v", result)
	}
	if !result.EditorialRefused {
		t.Fatalf("refusal not classified as editorial: %+v", result)
	}
	if result.EditorialReason != editorial.ReasonMissing {
		t.Fatalf("EditorialReason = %q, want %q", result.EditorialReason, editorial.ReasonMissing)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("origin/main advanced despite refused merge: before=%s after=%s", before, after)
	}

	if bd := readLog(t, bdLog); !strings.Contains(bd, "failure_class:precondition") {
		t.Fatalf("failure receipt missing failure_class:precondition, bd log:\n%s", bd)
	}
	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "nudge") || !strings.Contains(gtCalls, "test-rig/witness") {
		t.Fatalf("witness was not nudged, gt log:\n%s", gtCalls)
	}
}

// TestDoMergePR_EditorialRequired_ApproveNote_MergesAndCopiesNote covers the
// same path's success case: an approve note whose patch-id matches lets the
// provider merge proceed, and the note ends up readable on the commit the
// provider actually landed — not merely on the reviewed branch tip, which
// target's history never shows.
func TestDoMergePR_EditorialRequired_ApproveNote_MergesAndCopiesNote(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/editorial-pr-approve"
	createFeatureBranch(t, workDir, branch, "pr-editorial.txt", "hello\n")
	head := run(t, workDir, "git", "rev-parse", branch)

	base, err := g.MergeBase("origin/main", head)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	patchID, err := g.PatchID(base, head)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	approveNoteFor(t, g, "mr-editorial-pr-approve", "polecats/max", base, head, patchID)

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}
	e.prProvider = &recordingPRProvider{mergeFunc: func(method string) (string, error) {
		if method != "merge" {
			return "", fmt.Errorf("method = %s, want merge", method)
		}
		// The provider owns the landing on this path: merge and push on its
		// behalf, and report the merge commit it produced.
		run(t, workDir, "git", "checkout", "main")
		run(t, workDir, "git", "merge", "--no-ff", "-m", "merge PR", branch)
		run(t, workDir, "git", "push", "origin", "main")
		return run(t, workDir, "git", "rev-parse", "main"), nil
	}}

	mr := &MRInfo{
		ID:                    "mr-editorial-pr-approve",
		Branch:                branch,
		Target:                "main",
		Worker:                "polecats/max",
		CommitSHA:             head,
		EditorialReviewedHead: head,
	}
	result := e.doMergePR(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMergePR failed despite matching approve note: %s", result.Error)
	}

	landed := run(t, workDir, "git", "rev-parse", "origin/main")
	if landed != result.MergeCommit {
		t.Fatalf("origin/main = %s, reported merge commit = %s", landed, result.MergeCommit)
	}
	got, err := editorial.ReadNote(g, landed)
	if err != nil {
		t.Fatalf("note not readable at landed commit %s: %v", landed, err)
	}
	if got.MR != mr.ID {
		t.Fatalf("landed note is for %s, want %s", got.MR, mr.ID)
	}
	if got.PatchID != patchID {
		t.Fatalf("copied note patch-id mismatch: got %s, want %s", got.PatchID, patchID)
	}
	if strings.TrimSpace(run(t, workDir, "git", "ls-remote", "origin", "refs/notes/om")) == "" {
		t.Fatal("expected refs/notes/om to be pushed to origin alongside the PR merge")
	}
}

// TestProcessBatch_SingleMR_EditorialRefusal_LeftInQueueQuietly closes the
// second om-gate T6 precondition gap: a refusal used to come back as a bare
// ProcessResult{Error: ...}, so processSingleMR reported it as a generic
// failure — a batch error every cycle — and HandleMRInfoFailure then nudged
// the polecat and the mayor and started dead-worker recovery for a diff that
// is simply not approved yet. It must instead stay queued, quietly.
func TestProcessBatch_SingleMR_EditorialRefusal_LeftInQueueQuietly(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	branch := "polecat/test/editorial-single"
	createFeatureBranch(t, workDir, branch, "feature.txt", "hello\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	result := e.ProcessBatch(context.Background(), []*MRInfo{makeMR("mr-editorial-single", branch, "main")}, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("an editorial refusal was raised as a batch error: %v", result.Error)
	}
	if len(result.Merged) != 0 {
		t.Fatalf("expected nothing merged, got %v", mrIDs(result.Merged))
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("origin/main advanced despite refused push: before=%s after=%s", before, after)
	}

	// The witness still learns about the refusal (the design's escalation),
	// but the worker and the mayor are not told anything they could act on.
	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "test-rig/witness") {
		t.Fatalf("witness was not nudged, gt log:\n%s", gtCalls)
	}
	if strings.Contains(gtCalls, "MERGE_FAILED") || strings.Contains(gtCalls, "mayor/") {
		t.Fatalf("editorial refusal nudged the worker/mayor as a build failure, gt log:\n%s", gtCalls)
	}
	if bd := readLog(t, bdLog); !strings.Contains(bd, "failure_class:precondition") {
		t.Fatalf("failure receipt missing failure_class:precondition, bd log:\n%s", bd)
	}
}

// TestHandleMRInfoFailure_EditorialRefused_NoRecovery is the recovery half of
// the same gap: dead-worker recovery re-dispatches a worker to redo the
// branch, which for an unchanged refusal would spawn work on every cycle the
// refusal stands. A refusal the worker cannot act on must not reach it.
func TestHandleMRInfoFailure_EditorialRefused_NoRecovery(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	var buf bytes.Buffer
	e.output = &buf
	e.workDir = workDir

	recovered := false
	e.recoverDeadWorker = func(req deadWorkerRecoveryRequest) bool {
		recovered = true
		return true
	}

	mr := &MRInfo{
		ID:          "gt-mr-editorial",
		Branch:      "polecat/nux/gt-src1+abc123",
		Target:      "main",
		SourceIssue: "gt-src1",
		Worker:      "polecats/nux",
	}
	e.HandleMRInfoFailure(mr, ProcessResult{
		Success:          false,
		EditorialRefused: true,
		EditorialReason:  editorial.ReasonMissing,
		Error:            "editorial precondition failed for gt-mr-editorial: missing",
	})

	if recovered {
		t.Fatal("editorial refusal must not trigger dead-worker recovery")
	}
	output := buf.String()
	if !strings.Contains(output, "editorial precondition refused") {
		t.Fatalf("expected the refusal to be reported as an editorial one, got:\n%s", output)
	}
	if strings.Contains(output, "MERGE_FAILED") {
		t.Fatalf("editorial refusal must not report MERGE_FAILED, got:\n%s", output)
	}
}

// TestBatchPush_EditorialRequired_AllNotesApprove_LandsAndCopiesEachNote
// covers the multi-MR landing path's success case, which only ever had the
// refusal path (TestBatchPush_EditorialRequired_OneMissingNote_... below) and
// a presence-only check in batch_editorial_test.go behind it.
//
// buildLandedMRs reads the landed commits off origin/target..HEAD's
// first-parent chain — oldest first, one MergeNoFF commit per stacked member
// in merge order — and CopyNotesToLanded copies note i onto landed commit i.
// A presence-only assertion cannot see a mis-pairing, so this pins the
// pairing itself: the note on each landed commit must name the member whose
// branch that commit actually landed, cross-checked against the file that
// commit's own diff touches.
func TestBatchPush_EditorialRequired_AllNotesApprove_LandsAndCopiesEachNote(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	fileOf := map[string]string{"mr-batch-a": "a.txt", "mr-batch-b": "b.txt"}
	wantPatchID := map[string]string{}
	for _, id := range []string{"mr-batch-a", "mr-batch-b"} {
		branch := "feature-" + strings.TrimPrefix(id, "mr-batch-")
		createFeatureBranch(t, workDir, branch, fileOf[id], "hello "+id+"\n")

		head := run(t, workDir, "git", "rev-parse", branch)
		base, err := g.MergeBase("origin/main", head)
		if err != nil {
			t.Fatalf("MergeBase for %s: %v", id, err)
		}
		patchID, err := g.PatchID(base, head)
		if err != nil {
			t.Fatalf("PatchID for %s: %v", id, err)
		}
		wantPatchID[id] = patchID
		approveNoteFor(t, g, id, "polecats/max", base, head, patchID)
	}

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	batch := []*MRInfo{
		makeMR("mr-batch-a", "feature-a", "main"),
		makeMR("mr-batch-b", "feature-b", "main"),
	}
	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("BuildRebaseStack: stacked=%d conflicts=%d err=%v", len(stacked), len(conflicts), err)
	}
	if len(stacked) != 2 {
		t.Fatalf("expected 2 stacked MRs, got %d", len(stacked))
	}
	// The reviewed head the precondition reads. These notes were written
	// against each branch's own range, exactly as gt mq review writes them.
	for _, mr := range stacked {
		mr.EditorialReviewedHead = run(t, workDir, "git", "rev-parse", mr.Branch)
	}

	result := e.verifyAndPush(context.Background(), stacked, "main", nil)
	if result.Error != nil {
		t.Fatalf("batch push failed: %v (output:\n%s)", result.Error, e.output)
	}
	if len(result.Merged) != 2 {
		t.Fatalf("expected 2 merged, got %d (output:\n%s)", len(result.Merged), e.output)
	}

	// origin/main now equals HEAD, so the range buildLandedMRs used is empty;
	// the landed commits are the last len(stacked) first-parent commits, oldest
	// first — the same order and set buildLandedMRs saw before the push.
	landed := strings.Fields(run(t, workDir, "git", "rev-list", "--first-parent", "--max-count=2", "--reverse", "HEAD"))
	if len(landed) != 2 {
		t.Fatalf("expected 2 first-parent landed commits, got %d: %v", len(landed), landed)
	}
	for i, sha := range landed {
		id := stacked[i].ID
		note, err := editorial.ReadNote(g, sha)
		if err != nil {
			t.Fatalf("no editorial note on landed commit %d (%s): %v", i, sha, err)
		}
		if note.MR != id {
			t.Fatalf("landed commit %d (%s) carries %s's note, want %s's — note/MR pairing is off", i, sha, note.MR, id)
		}
		if note.PatchID != wantPatchID[id] {
			t.Fatalf("landed commit %d carries %s's note with patch-id %s, want %s", i, id, note.PatchID, wantPatchID[id])
		}
		changed := run(t, workDir, "git", "diff", "--name-only", sha+"^", sha)
		if !strings.Contains(changed, fileOf[id]) {
			t.Fatalf("landed commit %d (%s) changed %q, expected it to land %s for %s", i, sha, changed, fileOf[id], id)
		}
	}
}

// TestBatchPush_EditorialRequired_OneMissingNote_RefusesWholeBatchPush
// exercises the Task 6 push precondition at the batch level directly —
// stacking two MRs (BuildRebaseStack) and pushing them (verifyAndPush,
// which fastForwardBatch drives) — the same precondition check every batch
// push runs before the merge slot is acquired, refusing the whole push
// when any one stacked member fails it.
//
// This calls BuildRebaseStack/verifyAndPush directly rather than
// ProcessBatch: since om-gate T7 (batch_editorial.go), a ProcessBatch call
// with 2+ candidates runs editorial review on every candidate first and
// drops anything that doesn't come back approved — including, on a rig
// with no working harness configured (as here), every candidate — before
// the batch ever reaches this precondition. A batch that was truly never
// reviewed can no longer reach fastForwardBatch through ProcessBatch, so
// this test reaches it the way the single-MR path still can (doMerge
// bypasses T7 entirely — see TestDoMerge_EditorialRequired_NoNote_RefusesPush
// for that equivalent, ProcessBatch-independent coverage of the same
// precondition).
func TestBatchPush_EditorialRequired_OneMissingNote_RefusesWholeBatchPush(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	bdLog, gtLog := fakeBDAndGt(t)

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true}

	before := run(t, workDir, "git", "rev-parse", "origin/main")

	batch := []*MRInfo{
		makeMR("mr-batch-a", "feature-a", "main"),
		makeMR("mr-batch-b", "feature-b", "main"),
	}
	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("BuildRebaseStack: stacked=%d conflicts=%d err=%v", len(stacked), len(conflicts), err)
	}

	result := e.verifyAndPush(context.Background(), stacked, "main", nil)
	if result.Error == nil {
		t.Fatalf("expected batch push to be refused, got: %+v", result)
	}
	if !strings.Contains(result.Error.Error(), "editorial precondition failed for mr-batch-a") {
		t.Fatalf("expected error to name the failing MR, got: %v", result.Error)
	}
	if after := run(t, workDir, "git", "rev-parse", "origin/main"); after != before {
		t.Fatalf("origin/main advanced despite refused batch push: before=%s after=%s", before, after)
	}

	bd := readLog(t, bdLog)
	if !strings.Contains(bd, "failure_class:precondition") {
		t.Fatalf("failure receipt missing failure_class:precondition, bd log:\n%s", bd)
	}
	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "nudge") || !strings.Contains(gtCalls, "test-rig/witness") {
		t.Fatalf("witness was not nudged, gt log:\n%s", gtCalls)
	}
}

// TestHandleMRInfoSuccess_RubricChangeEscalatesToOperator is the batch
// path's half of the mayor's gt-7bvf design decision: before this, only the
// CLI's `gt mq post-merge` re-stamped the manifest after a rubric-touching
// merge, so a rubric change that landed through the batch path (this
// function) left the manifest stale with no escalation at all — the exact
// outage gt-7bvf exists to fix, on the one path nothing caught it. Now
// both paths detect the touch and escalate to the operator; neither
// restamps the manifest automatically (a rubric change never self-deploys).
func TestHandleMRInfoSuccess_RubricChangeEscalatesToOperator(t *testing.T) {
	_, gtLog := fakeBDAndGt(t)
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	const baseRubric = `{
  "threshold": 0.6,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."}
  ]
}`
	const loweredRubric = `{
  "threshold": 0.1,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."}
  ]
}`
	writeFile(t, workDir, ".om.json", baseRubric)
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "add rubric")
	run(t, workDir, "git", "push", "origin", "main")

	branch := "polecat/test/rubric-change"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	writeFile(t, workDir, ".om.json", loweredRubric)
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "lower threshold")
	commit := run(t, workDir, "git", "rev-parse", branch)
	run(t, workDir, "git", "push", "origin", branch)

	run(t, workDir, "git", "checkout", "main")
	run(t, workDir, "git", "merge", "--ff-only", branch)
	run(t, workDir, "git", "push", "origin", "main")
	mergeCommit := run(t, workDir, "git", "rev-parse", "main")

	// The manifest still pins the pre-change (base) rubric — exactly what it
	// holds right after this merge lands, since post-merge no longer
	// restamps automatically.
	baseSum := sha256.Sum256([]byte(baseRubric))
	baseSHA := hex.EncodeToString(baseSum[:])
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = ".om.json"
	manifest.Rubric.SHA256 = baseSHA
	if err := editorial.SaveManifest(workDir, manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	e := newTestEngineer(t, workDir, g)
	if !e.HandleMRInfoSuccess(&MRInfo{
		ID:        "mr-rubric-change",
		Branch:    branch,
		Target:    "main",
		CommitSHA: commit,
	}, ProcessResult{Success: true, MergeCommit: mergeCommit}) {
		t.Fatalf("HandleMRInfoSuccess failed:\n%s", e.output.(interface{ String() string }).String())
	}

	gtCalls := readLog(t, gtLog)
	if !strings.Contains(gtCalls, "escalate") {
		t.Fatalf("gt escalate was not invoked, gt log:\n%s", gtCalls)
	}
	if !strings.Contains(gtCalls, "rubric changed on main") {
		t.Fatalf("escalation message missing rubric-change context, gt log:\n%s", gtCalls)
	}

	reloaded, err := editorial.LoadManifest(workDir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if reloaded.Rubric.SHA256 != baseSHA {
		t.Errorf("manifest rubric sha = %s, want unchanged %s (no auto-restamp)", reloaded.Rubric.SHA256, baseSHA)
	}
}
