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
