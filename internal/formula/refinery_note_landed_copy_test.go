package formula

import (
	"strings"
	"testing"
)

// gt-fr3r: process-branch rehearses the merge on `temp` and `gt mq review`
// keys the approve note to that rehearsal head, while merge-push re-merges
// origin/<polecat-branch> from a target checkout and lands a different merge
// commit. The note therefore reaches neither the landed commit nor, since the
// coverage walk is first-parent only, the editorial-coverage check — so a
// landing this path performed reads as uncovered even though the diff it
// landed was reviewed and approved. The Go engineer path copies the note
// forward in doMerge (editorial.CopyNotesToLanded); the raw-git path has to
// ask for the same copy, and merge-push is where the landed sha is known.
//
// These assertions are text-level on purpose: the repair lives in formula
// text, which no compiler checks, so the step's guard, its auditability
// (--landed/--target/--reason, never a hand-written note) and its ordering
// ahead of cleanup are what a later edit can silently drop.
func TestRefineryPatrolMergePushRekeysNoteOntoLandedCommit(t *testing.T) {
	f := loadRefineryPatrolFormula(t)
	mergePush := requireFormulaStep(t, f, "merge-push")
	desc := mergePush.Description

	// Anchored on the actual invocation, not the bare command name: "gt mq
	// rekey-note" alone also appears earlier in this step's prose (explaining
	// why the editorial-coverage check and the backfill match on the same
	// patch-id) and in the later HELP-mail retry line, so a bare-name match
	// would find one of those instead and could pass even if the real
	// invocation moved after post-merge or vanished entirely.
	const rekeyInvocation = `gt mq rekey-note <mr-bead-id> --landed "$LANDED_SHA"`
	rekeyAt := strings.Index(desc, rekeyInvocation)
	if rekeyAt < 0 {
		t.Fatal("merge-push never re-keys the om verdict onto the landed commit; the landed merge commit is left without the note gt mq review wrote on the rehearsal head (gt-fr3r)")
	}
	postMergeAt := strings.Index(desc, "gt mq post-merge <rig> <mr-bead-id>")
	if postMergeAt < 0 {
		t.Fatal("merge-push no longer runs the post-merge command this test orders the re-key against")
	}
	if rekeyAt > postMergeAt {
		t.Error("the re-key runs after post-merge cleanup; it must run before the branch is deleted and before MERGED is sent, so the verdict is on the landed commit when the witness verifies the landing")
	}

	// The copy must be the audited one: rekey-note refuses when the patch-id
	// does not match, and --reason records why a post-hoc copy is legitimate.
	invocation := firstLineContaining(desc, "gt mq rekey-note <mr-bead-id>")
	for _, flag := range []string{"--landed", "--target", "--reason"} {
		if !strings.Contains(invocation, flag) {
			t.Errorf("re-key invocation %q is missing %s", invocation, flag)
		}
	}

	// Only rigs that gate landings with om have a note to copy at all.
	if !strings.Contains(desc, `{{editorial_required}}`) {
		t.Error("the re-key is not guarded on editorial_required, so it would run on rigs that never call gt mq review")
	}

	// A merge that creates nothing is not this MR's landing: LANDED_SHA is
	// then the previous MR's merge commit and re-keying onto it is a patch-id
	// mismatch, which would escalate on every already-contained branch.
	if !strings.Contains(desc, `git log -1 --format=%P "$LANDED_SHA"`) {
		t.Error("the re-key does not ask the landed commit for its parents, so it cannot tell a merge it created from a landing that already happened")
	}
	if !strings.Contains(desc, "MERGE_LANDED_HERE") {
		t.Error("the re-key has no guard for a branch the target already contained (no commit created), so that case would be reported as a missing-note failure")
	}

	// A verdict without proof is the defect this copy removes, so a copy that
	// fails must be loud: main has already moved and nothing can be refused
	// retroactively (gt-qvxf is the same asymmetry on the Go side).
	if !strings.Contains(desc, "EDITORIAL_RECORD_FAILED") {
		t.Error("a failed re-key is not marked on the step's output")
	}
	if !strings.Contains(desc, `gt mail send <rig>/witness -s "HELP: om note missing on landed commit`) {
		t.Error("a failed re-key does not escalate to the witness; the landed commit stays uncovered and nobody is told")
	}
}

// firstLineContaining returns the first line of s containing substr, or "".
func firstLineContaining(s, substr string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}
