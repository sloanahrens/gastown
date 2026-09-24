package formula

import (
	"strings"
	"testing"
)

// claude-7fc: gt mq post-merge now sends MERGED, archives MERGE_READY,
// attests the landed commit and deletes temp, then may respawn the refinery
// session. The formula must stop telling the agent to do those chores after
// post-merge (a respawn would cut them off, and a repeat is a duplicate
// MERGED mail), and must tell it to expect the respawn.
func TestRefineryPatrolPostMergeOwnsPerMRChores(t *testing.T) {
	f := loadRefineryPatrolFormula(t)

	mergePush := requireFormulaStep(t, f, "merge-push").Description
	batchScan := requireFormulaStep(t, f, "batch-scan").Description
	mergedSweep := requireFormulaStep(t, f, "merged-pr-sweep").Description

	// The fallback ✗ instructions still name `gt mail send <rig>/witness -s
	// "MERGED` as the one-off manual chore for a printed ✗, so that
	// substring's mere presence is not the defect. What must be gone is a
	// full leftover MERGED mail fence: every old fence in these three steps
	// carried `-m "Branch: <branch>` in its body, and none of the new text
	// does — post-merge / batch run compose and send that mail themselves.
	for id, desc := range map[string]string{"merge-push": mergePush, "batch-scan": batchScan, "merged-pr-sweep": mergedSweep} {
		if strings.Contains(desc, `-m "Branch: <branch>`) {
			t.Errorf("%s still has a full MERGED mail fence; post-merge / batch run sends MERGED itself", id)
		}
	}
	if strings.Contains(mergePush, "gt mail archive <merge-ready-message-id>") {
		t.Error("merge-push still tells the agent to archive MERGE_READY; post-merge archives it")
	}
	if strings.Contains(mergePush, `bd comments add <mr-bead-id> "post-merge: attested`) {
		t.Error("merge-push still adds the attestation comment by hand; post-merge adds it")
	}
	for _, want := range []string{"✓ MERGED sent", "✓ MERGE_READY archived", "✓ temp branch deleted", "respawn"} {
		if !strings.Contains(mergePush, want) {
			t.Errorf("merge-push does not tell the agent to verify/expect %q", want)
		}
	}
	// The re-key must still precede post-merge (refinery_note_landed_copy_test).
	if !strings.Contains(mergePush, "gt mq post-merge <rig> <mr-bead-id>") {
		t.Fatal("merge-push lost its post-merge invocation")
	}
}

// MR-B final review (I1, M1, M3, M4): batch recovery must not respawn the
// session, no step may point at the deleted merge-push "Step 4 (archive
// mail)", no step may tell the agent to redo post-merge's chores by hand
// except as the ✗ fallback, and the gate must accept post-merge's ○ lines.
func TestRefineryPatrolUnitCycleReviewFixes(t *testing.T) {
	f := loadRefineryPatrolFormula(t)

	all := ""
	for _, s := range f.Steps {
		all += s.Description + "\n"
	}
	for _, stale := range []string{
		"Step 4 (archive mail)",
		"to send MERGED notification",
		"MERGED mail was sent to witness",
		"If notifications or archiving were\nmissed, do them now",
	} {
		if strings.Contains(all, stale) {
			t.Errorf("formula still contains stale chore text %q", stale)
		}
	}

	batchScan := requireFormulaStep(t, f, "batch-scan").Description
	if !strings.Contains(batchScan, "gt mq post-merge <rig> <mr-id> --skip-branch-delete --no-cycle") {
		t.Error("batch-scan recovery post-merge does not pass --no-cycle; the first recovered member could respawn the session")
	}
	escalate := strings.Index(batchScan, "mail the mayor) FIRST")
	recoverAt := strings.Index(batchScan, "--skip-branch-delete --no-cycle")
	if escalate < 0 || recoverAt < 0 || escalate > recoverAt {
		t.Error("batch-scan must escalate an infra .error to the mayor before the recovery post-merges")
	}

	mergedSweep := requireFormulaStep(t, f, "merged-pr-sweep").Description
	if !strings.Contains(mergedSweep, "successor re-runs this sweep") {
		t.Error("merged-pr-sweep does not say a respawn there is expected and the successor continues")
	}

	mergePush := requireFormulaStep(t, f, "merge-push").Description
	for _, want := range []string{"○ no temp branch to delete", "`○` lines never need action"} {
		if !strings.Contains(mergePush, want) {
			t.Errorf("merge-push does not accept post-merge's %q", want)
		}
	}
}
