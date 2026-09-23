package refinery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/checkpoint"
)

// TestDoMergeSquashesAutoCheckpointCommit reproduces gt-rswr: a branch whose
// entire payload rides on a single checkpoint_dog commit (the polecat never
// amended the generic subject before gt done). Merging it with --no-ff would
// carry that commit — and its "WIP: checkpoint (auto)" subject — straight
// onto main. doMerge must squash it instead so main's history never gains it,
// while still landing the branch's content.
func TestDoMergeSquashesAutoCheckpointCommit(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/checkpoint-only"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	writeFile(t, workDir, "feature.txt", "the whole fix\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", checkpoint.WIPCommitPrefix)
	run(t, workDir, "git", "checkout", "main")

	e := newTestEngineer(t, workDir, g)
	mr := &MRInfo{ID: "mr-checkpoint-only", Branch: branch, Target: "main"}
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed: %s\n%s", result.Error, engineerOutput(t, e))
	}

	history := run(t, workDir, "git", "log", "--oneline", "origin/main")
	if strings.Contains(history, checkpoint.WIPCommitPrefix) {
		t.Errorf("origin/main history still carries a %q commit:\n%s", checkpoint.WIPCommitPrefix, history)
	}

	tipSubject := run(t, workDir, "git", "log", "-1", "--format=%s", "origin/main")
	if checkpoint.IsAutoSaveSubject(tipSubject) {
		t.Errorf("landed merge commit subject %q is itself machine-generated", tipSubject)
	}

	content, err := os.ReadFile(filepath.Join(workDir, "feature.txt"))
	if err != nil {
		t.Fatalf("feature.txt missing from merged tree: %v", err)
	}
	if string(content) != "the whole fix\n" {
		t.Errorf("feature.txt content = %q, want the branch's payload", content)
	}
}

// TestDoMergeSquashesAutoCheckpointCommitAmongOthers covers a branch where the
// checkpoint commit is not the tip: a later, descriptive commit follows it.
// The tip subject is already fine, but the checkpoint commit must still be
// squashed away rather than riding along in target's ancestry.
func TestDoMergeSquashesAutoCheckpointCommitAmongOthers(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/checkpoint-then-fix"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	writeFile(t, workDir, "feature.txt", "draft\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", checkpoint.WIPCommitPrefix)
	writeFile(t, workDir, "feature.txt", "final\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "fix: finish the feature")
	run(t, workDir, "git", "checkout", "main")

	e := newTestEngineer(t, workDir, g)
	mr := &MRInfo{ID: "mr-checkpoint-then-fix", Branch: branch, Target: "main"}
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed: %s\n%s", result.Error, engineerOutput(t, e))
	}

	history := run(t, workDir, "git", "log", "--oneline", "origin/main")
	if strings.Contains(history, checkpoint.WIPCommitPrefix) {
		t.Errorf("origin/main history still carries a %q commit:\n%s", checkpoint.WIPCommitPrefix, history)
	}

	tipSubject := run(t, workDir, "git", "log", "-1", "--format=%s", "origin/main")
	if tipSubject != "fix: finish the feature" {
		t.Errorf("landed merge commit subject = %q, want the branch's descriptive tip preserved", tipSubject)
	}

	content, err := os.ReadFile(filepath.Join(workDir, "feature.txt"))
	if err != nil {
		t.Fatalf("feature.txt missing from merged tree: %v", err)
	}
	if string(content) != "final\n" {
		t.Errorf("feature.txt content = %q, want the branch's final payload", content)
	}
}

// TestDoMergeSquashPassesPostMergeProof confirms the squash path used for a
// branch carrying an auto-checkpoint commit still satisfies the post-merge
// proof check every successful merge goes through (HandleMRInfoSuccess calls
// verifyMRInfoPostMergeProof, which requires the submitted commit_sha to be
// an ancestor of — or tree-preserved on — target). A squash-merged commit is
// not a literal git ancestor of the submitted head, so this exercises the
// merge-tree-noop fallback rather than assuming it.
func TestDoMergeSquashPassesPostMergeProof(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/checkpoint-proof"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	writeFile(t, workDir, "feature.txt", "the whole fix\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", checkpoint.WIPCommitPrefix)
	commit := run(t, workDir, "git", "rev-parse", branch)
	run(t, workDir, "git", "checkout", "main")

	e := newTestEngineer(t, workDir, g)
	mr := &MRInfo{ID: "mr-checkpoint-proof", Branch: branch, Target: "main", CommitSHA: commit}
	result := e.doMerge(context.Background(), mr)
	if !result.Success {
		t.Fatalf("doMerge failed: %s\n%s", result.Error, engineerOutput(t, e))
	}

	if err := e.verifyMRInfoPostMergeProof(mr); err != nil {
		t.Errorf("post-merge proof failed after squash merge: %v\n%s", err, engineerOutput(t, e))
	}
}

// TestProcessBatchSquashesAutoCheckpointCommit covers the batch half of the
// same guarantee. Stacking merges each member itself (BuildRebaseStack), so
// the single-MR squash above never sees a batched branch — which is how a
// member's checkpoints still rode a stacked merge onto main (gt-gfdl:
// 80d93e2d, a stacked --no-ff merge carrying two "WIP: checkpoint (auto)"
// commits beneath the member's real tip). The batch must keep them off
// target without losing the branch's content.
func TestProcessBatchSquashesAutoCheckpointCommit(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	// The shape that landed: real fix at the tip, checkpoints beneath it.
	branch := "polecat/test/batch-checkpoint"
	run(t, workDir, "git", "checkout", "-b", branch, "main")
	writeFile(t, workDir, "feature.txt", "draft\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", checkpoint.WIPCommitPrefix)
	writeFile(t, workDir, "feature.txt", "almost done\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", checkpoint.WIPCommitPrefix)
	writeFile(t, workDir, "feature.txt", "the whole fix\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "fix: finish the feature")
	run(t, workDir, "git", "checkout", "main")

	// A second member, so the batch takes the stacking path rather than
	// degrading to the single-MR one.
	createFeatureBranch(t, workDir, "feature-other", "other.txt", "unrelated\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-checkpoint", branch, "main"),
		makeMR("mr-other", "feature-other", "main"),
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("ProcessBatch failed: %v\n%s", result.Error, engineerOutput(t, e))
	}
	if len(result.Merged) != 2 {
		// result.Merged holds only members that passed HandleMRInfoSuccess, so
		// this also asserts the squashed member survives the post-merge proof
		// without the submitted head sitting in target's ancestry.
		t.Fatalf("expected both MRs merged, got %d\n%s", len(result.Merged), engineerOutput(t, e))
	}

	subjects := run(t, workDir, "git", "log", "--format=%s", "origin/main")
	for _, subject := range strings.Split(subjects, "\n") {
		if checkpoint.IsAutoSaveSubject(strings.TrimSpace(subject)) {
			t.Errorf("origin/main gained a machine-generated commit %q:\n%s", subject, subjects)
		}
	}
	if !strings.Contains(subjects, "fix: finish the feature") {
		t.Errorf("the member's own subject was lost from origin/main:\n%s", subjects)
	}

	content, err := os.ReadFile(filepath.Join(workDir, "feature.txt"))
	if err != nil {
		t.Fatalf("feature.txt missing from merged tree: %v", err)
	}
	if string(content) != "the whole fix\n" {
		t.Errorf("feature.txt content = %q, want the branch's payload", content)
	}
	if _, err := os.Stat(filepath.Join(workDir, "other.txt")); err != nil {
		t.Errorf("the batched MR's file did not land: %v", err)
	}
}
