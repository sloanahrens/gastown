package polecat

import (
	"os/exec"
	"strings"
	"testing"
)

// gt-0kk2. Reusing an idle polecat with --branch prepares the shared worktree by
// resetting it to the resume branch's origin tip. The reset used to run while
// whatever branch the worktree happened to hold was still checked out, which in
// a shared .repo.git moves that ref — someone else's in-flight branch — onto the
// resume target (gt-9ed0: quartz's branch was reset onto jasper's origin tip
// mid-session, and only the reflog could say what it used to point at).
//
// This is the crossed swap the bead asks for: the polecat being reused holds
// another bead's branch, and the branch it is reused onto belongs to a third
// bead. Neither ref may move except as the resume itself intends.

// TestReuseIdlePolecat_CrossedSwap_LeavesHeldBranchRefAlone is the regression:
// the branch the worktree was holding must still name its own commit after reuse.
func TestReuseIdlePolecat_CrossedSwap_LeavesHeldBranchRefAlone(t *testing.T) {
	mgr, mayorRig := setupCanonicalBranchManagerTest(t)

	alpha, err := mgr.AddWithOptions("alpha", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}

	mainSHA := gitProbeOutput(t, mayorRig, "rev-parse", "origin/main")

	// alpha's worktree starts out holding another polecat's branch, the state the
	// gt-9ed0 session woke up in. Its tip is already on origin (and at the tip of
	// main, so the reuse gate still reads the slot as reusable — the reset
	// happened inside a reuse that the gate had cleared).
	heldBranch := "polecat/quartz/gt-9ed0+mudclpwf"
	runGit(t, mayorRig, "update-ref", "refs/heads/"+heldBranch, mainSHA)
	runGit(t, mayorRig, "update-ref", "refs/remotes/origin/"+heldBranch, mainSHA)
	runGit(t, alpha.ClonePath, "checkout", heldBranch)

	heldBefore := gitProbeOutput(t, alpha.ClonePath, "rev-parse", "refs/heads/"+heldBranch)

	// The resume target: a different bead's branch, one commit ahead of main.
	resumeBranch := "polecat/jasper/gt-bagu+mudcl5gv"
	resumeSHA := branchAtNewCommit(t, mayorRig, resumeBranch, mainSHA, "bagu work (gt-bagu)")
	runGit(t, mayorRig, "update-ref", "refs/remotes/origin/"+resumeBranch, resumeSHA)

	reused, err := mgr.ReuseIdlePolecat("alpha", AddOptions{
		HookBead:     "gt-next",
		ResumeBranch: resumeBranch,
	})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat: %v", err)
	}

	heldAfter := gitProbeOutput(t, reused.ClonePath, "rev-parse", "refs/heads/"+heldBranch)
	if heldAfter != heldBefore {
		t.Errorf("refs/heads/%s moved from %s to %s during reuse — a polecat must never move the branch it merely holds",
			heldBranch, heldBefore, heldAfter)
	}

	if reused.Branch != resumeBranch {
		t.Errorf("reused branch = %q, want %q", reused.Branch, resumeBranch)
	}
	if head := gitProbeOutput(t, reused.ClonePath, "symbolic-ref", "--short", "HEAD"); head != resumeBranch {
		t.Errorf("HEAD = %q, want %q", head, resumeBranch)
	}
	if tip := gitProbeOutput(t, reused.ClonePath, "rev-parse", "HEAD"); tip != resumeSHA {
		t.Errorf("HEAD = %s, want the resume branch's origin tip %s", tip, resumeSHA)
	}
	if status := gitProbeOutput(t, reused.ClonePath, "status", "--porcelain"); status != "" {
		t.Errorf("reused worktree is dirty:\n%s", status)
	}
}

// TestReuseIdlePolecat_RefusesBranchHeldByAnotherWorktree covers the other half
// of the crossed swap: when the resume target is checked out in a different
// worktree, reuse must refuse loudly and leave both worktrees untouched.
func TestReuseIdlePolecat_RefusesBranchHeldByAnotherWorktree(t *testing.T) {
	mgr, mayorRig := setupCanonicalBranchManagerTest(t)

	alpha, err := mgr.AddWithOptions("alpha", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions(alpha): %v", err)
	}
	beta, err := mgr.AddWithOptions("beta", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions(beta): %v", err)
	}

	mainSHA := gitProbeOutput(t, mayorRig, "rev-parse", "origin/main")

	// The branch beta is live on. Resuming it in alpha would put two worktrees on
	// one ref, which is how the crossed state that stranded gt-9ed0 was built.
	heldBranch := "polecat/alpha/gt-x+aaa"
	runGit(t, mayorRig, "update-ref", "refs/heads/"+heldBranch, mainSHA)
	runGit(t, mayorRig, "update-ref", "refs/remotes/origin/"+heldBranch, mainSHA)
	runGit(t, beta.ClonePath, "checkout", heldBranch)

	alphaBefore := gitProbeOutput(t, alpha.ClonePath, "symbolic-ref", "--short", "HEAD")

	_, err = mgr.ReuseIdlePolecat("alpha", AddOptions{
		HookBead:     "gt-next",
		ResumeBranch: heldBranch,
	})
	if err == nil {
		t.Fatal("ReuseIdlePolecat resumed a branch checked out in another worktree; want refusal")
	}
	if !strings.Contains(err.Error(), "already checked out at") {
		t.Fatalf("refusal does not explain the conflict: %v", err)
	}
	if holder := holderFromRefusal(t, err.Error()); !sameWorktreePath(holder, beta.ClonePath) {
		t.Errorf("refusal named holder %q, want beta's worktree %q", holder, beta.ClonePath)
	}

	// Refusing must be inert: alpha keeps the branch it had, and neither ref moved.
	if alphaAfter := gitProbeOutput(t, alpha.ClonePath, "symbolic-ref", "--short", "HEAD"); alphaAfter != alphaBefore {
		t.Errorf("alpha's checked-out branch changed from %q to %q on a refused reuse", alphaBefore, alphaAfter)
	}
	if betaHead := gitProbeOutput(t, beta.ClonePath, "symbolic-ref", "--short", "HEAD"); betaHead != heldBranch {
		t.Errorf("beta's checked-out branch = %q, want %q", betaHead, heldBranch)
	}
	if tip := gitProbeOutput(t, beta.ClonePath, "rev-parse", "refs/heads/"+heldBranch); tip != mainSHA {
		t.Errorf("refs/heads/%s moved to %s on a refused reuse, want %s", heldBranch, tip, mainSHA)
	}
}

// branchAtNewCommit creates branch at a fresh commit parented on ref, without
// touching any working tree.
func branchAtNewCommit(t *testing.T, repo, branch, parent, message string) string {
	t.Helper()

	tree := gitProbeOutput(t, repo, "rev-parse", parent+"^{tree}")
	sha := gitProbeOutput(t, repo,
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"commit-tree", tree, "-p", parent, "-m", message)
	runGit(t, repo, "update-ref", "refs/heads/"+branch, sha)
	return sha
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

func gitProbeOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}
