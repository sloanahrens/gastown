package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gt-0kk2: no polecat reuse may move a branch it does not own.

// TestReuseIdlePolecat_CrossedSwap_LeavesHeldBranchRefAlone is the regression:
// the branch the worktree was holding must still name its own commit after reuse.
func TestReuseIdlePolecat_CrossedSwap_LeavesHeldBranchRefAlone(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, added, w := canonicalWithPolecats(t, false, "alpha")
	alpha := added["alpha"]

	mainSHA := w.rev(t, mayorRig, "origin/main")

	// alpha's worktree starts out holding another polecat's branch, the state the
	// gt-9ed0 session woke up in. Its tip is already on origin (and at the tip of
	// main, so the reuse gate still reads the slot as reusable — the reset
	// happened inside a reuse that the gate had cleared).
	heldBranch := "polecat/quartz/gt-9ed0+mudclpwf"
	w.SetRef(t, mayorRig, "refs/heads/"+heldBranch, mainSHA)
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+heldBranch, mainSHA)
	w.switchTo(t, alpha.ClonePath, heldBranch)

	heldBefore := w.rev(t, alpha.ClonePath, "refs/heads/"+heldBranch)

	// The resume target: a different bead's branch, one commit ahead of main.
	resumeBranch := "polecat/jasper/gt-bagu+mudcl5gv"
	resumeSHA := w.branchAtNewCommit(t, mayorRig, resumeBranch, mainSHA, "bagu work (gt-bagu)")
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+resumeBranch, resumeSHA)

	reused, err := mgr.ReuseIdlePolecat("alpha", AddOptions{
		HookBead:     "gt-next",
		ResumeBranch: resumeBranch,
	})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat: %v", err)
	}

	heldAfter := w.rev(t, reused.ClonePath, "refs/heads/"+heldBranch)
	if heldAfter != heldBefore {
		t.Errorf("refs/heads/%s moved from %s to %s during reuse — a polecat must never move the branch it merely holds",
			heldBranch, heldBefore, heldAfter)
	}

	if reused.Branch != resumeBranch {
		t.Errorf("reused branch = %q, want %q", reused.Branch, resumeBranch)
	}
	if head := w.branch(t, reused.ClonePath); head != resumeBranch {
		t.Errorf("HEAD = %q, want %q", head, resumeBranch)
	}
	if tip := w.rev(t, reused.ClonePath, "HEAD"); tip != resumeSHA {
		t.Errorf("HEAD = %s, want the resume branch's origin tip %s", tip, resumeSHA)
	}
	if w.dirty(t, reused.ClonePath) {
		t.Errorf("reused worktree is dirty")
	}
}

// TestReuseIdlePolecat_FreshSling_LeavesHeldBranchRefAlone covers the fresh path
// (no --branch): a reuse resets to origin/main, and the branch the worktree was
// holding must not follow it there.
func TestReuseIdlePolecat_FreshSling_LeavesHeldBranchRefAlone(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, added, w := canonicalWithPolecats(t, false, "alpha")
	alpha := added["alpha"]

	mainSHA := w.rev(t, mayorRig, "origin/main")

	// alpha's worktree holds another bead's branch, one commit ahead of main so a
	// reset to origin/main would be visible in the ref.
	heldBranch := "polecat/pearl/gt-mjll+mud9574c"
	heldSHA := w.branchAtNewCommit(t, mayorRig, heldBranch, mainSHA, "pearl work (gt-mjll)")
	w.switchTo(t, alpha.ClonePath, heldBranch)

	reused, err := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: "gt-next"})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat: %v", err)
	}

	heldAfter := w.rev(t, reused.ClonePath, "refs/heads/"+heldBranch)
	if heldAfter != heldSHA {
		t.Errorf("refs/heads/%s moved from %s to %s during a fresh sling — the reuse must not rewrite the branch it merely held",
			heldBranch, heldSHA, heldAfter)
	}
	if reused.Branch == heldBranch {
		t.Errorf("fresh sling reused branch %q instead of creating one", heldBranch)
	}
	if tip := w.rev(t, reused.ClonePath, "HEAD"); tip != mainSHA {
		t.Errorf("HEAD = %s, want the fresh branch at origin/main %s", tip, mainSHA)
	}
}

// TestReuseIdlePolecat_RefusesBranchHeldByAnotherWorktree covers the other half
// of the crossed swap: when the resume target is checked out in a different
// worktree, reuse must refuse loudly and leave both worktrees untouched.
func TestReuseIdlePolecat_RefusesBranchHeldByAnotherWorktree(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, added, w := canonicalWithPolecats(t, false, "alpha")
	alpha := added["alpha"]
	beta, err := mgr.AddWithOptions("beta", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions(beta): %v", err)
	}

	mainSHA := w.rev(t, mayorRig, "origin/main")

	// The branch beta is live on. Resuming it in alpha would put two worktrees on
	// one ref, which is how the crossed state that stranded gt-9ed0 was built.
	heldBranch := "polecat/alpha/gt-x+aaa"
	w.SetRef(t, mayorRig, "refs/heads/"+heldBranch, mainSHA)
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+heldBranch, mainSHA)
	w.switchTo(t, beta.ClonePath, heldBranch)
	// Uncommitted edits make beta live work. A clean idle holder is released
	// instead of refused (gt-l9td).
	dirtyWorktree(t, beta.ClonePath)

	alphaBefore := w.branch(t, alpha.ClonePath)

	_, err = mgr.ReuseIdlePolecat("alpha", AddOptions{
		HookBead:     "gt-next",
		ResumeBranch: heldBranch,
	})
	if err == nil {
		t.Fatal("ReuseIdlePolecat resumed a branch checked out in another worktree; want refusal")
	}
	if !errors.Is(err, ErrBranchHeld) {
		t.Fatalf("refusal is not ErrBranchHeld, so callers cannot tell it from a recoverable failure: %v", err)
	}
	if !strings.Contains(err.Error(), "already checked out at") {
		t.Fatalf("refusal does not explain the conflict: %v", err)
	}
	if holder := holderFromRefusal(t, err.Error()); !sameWorktreePath(holder, beta.ClonePath) {
		t.Errorf("refusal named holder %q, want beta's worktree %q", holder, beta.ClonePath)
	}

	// Refusing must be inert: alpha keeps the branch it had, and neither ref moved.
	if alphaAfter := w.branch(t, alpha.ClonePath); alphaAfter != alphaBefore {
		t.Errorf("alpha's checked-out branch changed from %q to %q on a refused reuse", alphaBefore, alphaAfter)
	}
	if betaHead := w.branch(t, beta.ClonePath); betaHead != heldBranch {
		t.Errorf("beta's checked-out branch = %q, want %q", betaHead, heldBranch)
	}
	if tip := w.rev(t, beta.ClonePath, "refs/heads/"+heldBranch); tip != mainSHA {
		t.Errorf("refs/heads/%s moved to %s on a refused reuse, want %s", heldBranch, tip, mainSHA)
	}
}

// TestAddWithOptions_RefusesResumeBranchHeldByAnotherWorktree covers the fallback
// a refused reuse lands on: attaching a brand-new worktree to the held branch
// with `worktree add --force`, which git permits and so needs the same check.
func TestAddWithOptions_RefusesResumeBranchHeldByAnotherWorktree(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, added, w := canonicalWithPolecats(t, false, "alpha")
	alpha := added["alpha"]

	mainSHA := w.rev(t, mayorRig, "origin/main")
	heldBranch := "polecat/alpha/gt-x+aaa"
	w.SetRef(t, mayorRig, "refs/heads/"+heldBranch, mainSHA)
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+heldBranch, mainSHA)
	w.switchTo(t, alpha.ClonePath, heldBranch)
	dirtyWorktree(t, alpha.ClonePath)

	_, err := mgr.AddWithOptions("beta", AddOptions{HookBead: "gt-next", ResumeBranch: heldBranch})
	if err == nil {
		t.Fatal("AddWithOptions attached a second worktree to a held branch; want refusal")
	}
	if !errors.Is(err, ErrBranchHeld) {
		t.Fatalf("refusal is not ErrBranchHeld: %v", err)
	}

	// The refused allocation must roll back: no beta directory left to occupy a slot.
	if mgr.exists("beta") {
		t.Error("refused AddWithOptions left beta behind")
	}
	if head := w.branch(t, alpha.ClonePath); head != heldBranch {
		t.Errorf("alpha's checked-out branch = %q, want %q", head, heldBranch)
	}
}

// TestHeldByOtherWorktree_FailsClosedOnUnreadableList pins the failure path: a
// worktree list that cannot be read must refuse, not report the branch free.
func TestHeldByOtherWorktree_FailsClosedOnUnreadableList(t *testing.T) {
	t.Parallel()
	notARepo := t.TempDir()
	branch := "polecat/other/gt-x+aaa"

	err := heldByOtherWorktree(newWorld().repo(notARepo), branch)
	if err == nil {
		t.Fatal("unreadable worktree list reported the branch as free; want a refusal")
	}
	if !errors.Is(err, ErrBranchHeld) {
		t.Fatalf("want ErrBranchHeld, got %v", err)
	}
	if !strings.Contains(err.Error(), branch) {
		t.Errorf("refusal does not name the branch: %v", err)
	}
}

// TestReuseIdlePolecat_IgnoresPrunableHolder covers a registration whose
// directory is gone: git still lists the branch for it, but there is no HEAD
// there to conflict with, and refusing would strand the resume until someone
// pruned the stale entry by hand.
func TestReuseIdlePolecat_IgnoresPrunableHolder(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, w := canonicalRig(t)
	mainSHA := w.rev(t, mayorRig, "origin/main")

	// The holder: a worktree deleted without being pruned, left on the branch.
	deadPath := filepath.Join(t.TempDir(), "stale-worktree")
	resumeBranch := "polecat/jasper/gt-bagu+mudcl5gv"
	resumeSHA := w.branchAtNewCommit(t, mayorRig, resumeBranch, mainSHA, "bagu work (gt-bagu)")
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+resumeBranch, resumeSHA)
	if err := w.OpenWorktreeRepo(mayorRig).WorktreeAddDetached(deadPath, mainSHA); err != nil {
		t.Fatal(err)
	}
	w.switchTo(t, deadPath, resumeBranch)
	if err := os.RemoveAll(deadPath); err != nil {
		t.Fatalf("removing the holder's directory: %v", err)
	}

	alpha, err := mgr.AddWithOptions("alpha", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}

	reused, err := mgr.ReuseIdlePolecat("alpha", AddOptions{
		HookBead:     "gt-next",
		ResumeBranch: resumeBranch,
	})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat refused a branch whose only holder is a deleted worktree: %v", err)
	}
	if tip := w.rev(t, reused.ClonePath, "HEAD"); tip != resumeSHA {
		t.Errorf("HEAD = %s, want the resume branch's origin tip %s (worktree %s)", tip, resumeSHA, alpha.ClonePath)
	}
	// The stale registration is pruned, not just ignored: git 2.50 refuses the
	// checkout above while it exists, and 2.42 does not, so this is the
	// assertion that holds the fix on either version (gt-22hdp.39).
	list, err := w.repo(mayorRig).WorktreeList()
	if err != nil {
		t.Fatal(err)
	}
	for _, wt := range list {
		if wt.Path == deadPath {
			t.Errorf("the deleted holder is still registered: %+v", list)
		}
	}
}

// holderFromRefusal pulls the holding worktree path out of a refusal message.
func holderFromRefusal(t *testing.T, message string) string {
	t.Helper()

	const marker = "already checked out at "
	idx := strings.Index(message, marker)
	if idx < 0 {
		t.Fatalf("no holder path in refusal: %s", message)
	}
	holder := message[idx+len(marker):]
	if nl := strings.IndexByte(holder, '\n'); nl >= 0 {
		holder = holder[:nl]
	}
	return holder
}
