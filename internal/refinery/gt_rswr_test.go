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
