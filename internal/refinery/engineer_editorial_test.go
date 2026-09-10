package refinery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
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
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/editorial-approve"
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

// TestDoMerge_EditorialRequired_ReviewedHeadDiffersFromLandedCommit_NoteCopied
// covers the copy-on-differing-head path: the reviewed head (from a rebase
// that left the diff unchanged) differs from both the submitted branch tip
// and the actual landed commit (doMerge always lands a new MergeNoFF
// commit), yet the matching patch-id lets the push through, and the note
// ends up readable at the commit that actually landed on origin/main —
// not at the submitted branch tip, which never carries it.
func TestDoMerge_EditorialRequired_ReviewedHeadDiffersFromLandedCommit_NoteCopied(t *testing.T) {
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

// TestProcessBatch_EditorialRequired_OneMissingNote_RefusesWholeBatchPush
// exercises the batch path named in Task 6: verifyAndPush/fastForwardBatch
// checks the precondition for every stacked member before the merge slot
// is acquired, and refuses the whole batch's push when any one fails —
// per-member ejection before stacking is a later task (T7).
func TestProcessBatch_EditorialRequired_OneMissingNote_RefusesWholeBatchPush(t *testing.T) {
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
	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
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
