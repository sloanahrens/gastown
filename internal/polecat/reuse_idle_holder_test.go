package polecat

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// gt-l9td: a resume must not be refused because the branch's author polecat is
// idle but not reaped. Only a holder with something to lose still refuses.

// dirtyWorktree leaves an uncommitted edit to a tracked file in dir.
func dirtyWorktree(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# edited\n"), 0o644); err != nil {
		t.Fatalf("dirty %s: %v", dir, err)
	}
}

// runGitOutput runs git in dir and returns its output and error, for probes
// whose failure is the answer.
func runGitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// idleHolderFixture is a rig where beta, an idle polecat, holds heldBranch,
// which origin also has at main's commit. alpha is the polecat that resumes it.
func idleHolderFixture(t *testing.T) (mgr *Manager, mayorRig string, alpha, beta *Polecat, heldBranch, tip string) {
	t.Helper()
	mgr, mayorRig, _, added := setupCanonicalWithPolecats(t, false, "alpha", "beta")
	alpha, beta = added["alpha"], added["beta"]

	tip = gitProbeOutput(t, mayorRig, "rev-parse", "origin/main")
	heldBranch = "polecat/beta/gt-x+aaa"
	runGit(t, mayorRig, "update-ref", "refs/heads/"+heldBranch, tip)
	runGit(t, mayorRig, "update-ref", "refs/remotes/origin/"+heldBranch, tip)
	runGit(t, beta.ClonePath, "checkout", heldBranch)
	return mgr, mayorRig, alpha, beta, heldBranch, tip
}

func TestReuseIdlePolecat_ResumesBranchHeldByIdleUnreapedPolecat(t *testing.T) {
	t.Parallel()
	mgr, _, alpha, beta, heldBranch, tip := idleHolderFixture(t)

	reused, err := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: "gt-next", ResumeBranch: heldBranch})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat refused a branch held by an idle polecat: %v", err)
	}
	if reused.ClonePath != alpha.ClonePath {
		t.Errorf("reused %s, want alpha's worktree %s", reused.ClonePath, alpha.ClonePath)
	}
	if head := gitProbeOutput(t, alpha.ClonePath, "symbolic-ref", "--short", "HEAD"); head != heldBranch {
		t.Errorf("alpha HEAD = %q, want %q", head, heldBranch)
	}

	// beta gave up the checkout, not the branch, and stays a reusable slot.
	if out, err := runGitOutput(beta.ClonePath, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Errorf("beta is still on a branch (%s), want a detached HEAD", out)
	}
	if got := gitProbeOutput(t, beta.ClonePath, "rev-parse", "HEAD"); got != tip {
		t.Errorf("beta HEAD = %s, want %s", got, tip)
	}
	if got := gitProbeOutput(t, alpha.ClonePath, "rev-parse", "refs/heads/"+heldBranch); got != tip {
		t.Errorf("refs/heads/%s = %s, want %s", heldBranch, got, tip)
	}
	if decision := mgr.ReuseDecisionForPolecat("beta", StateIdle); !decision.Reusable {
		t.Errorf("beta is not reusable after release: %s", decision.Reason)
	}
}

func TestAddWithOptions_ResumesBranchHeldByIdleUnreapedPolecat(t *testing.T) {
	t.Parallel()
	mgr, _, _, beta, heldBranch, _ := idleHolderFixture(t)

	gamma, err := mgr.AddWithOptions("gamma", AddOptions{HookBead: "gt-next", ResumeBranch: heldBranch})
	if err != nil {
		t.Fatalf("AddWithOptions refused a branch held by an idle polecat: %v", err)
	}
	if head := gitProbeOutput(t, gamma.ClonePath, "symbolic-ref", "--short", "HEAD"); head != heldBranch {
		t.Errorf("gamma HEAD = %q, want %q", head, heldBranch)
	}
	if _, err := runGitOutput(beta.ClonePath, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Error("beta is still on the branch gamma resumed")
	}
}

// TestReuseIdlePolecat_KeepsRefusingHolderWithSomethingToLose covers each way a
// holder stops being an idle slot: the release must leave it untouched.
func TestReuseIdlePolecat_KeepsRefusingHolderWithSomethingToLose(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(t *testing.T, mgr *Manager, mayorRig string, beta *Polecat, heldBranch string)
	}{
		{
			name: "uncommitted edits",
			setup: func(t *testing.T, _ *Manager, _ string, beta *Polecat, _ string) {
				dirtyWorktree(t, beta.ClonePath)
			},
		},
		{
			name: "stashed work",
			setup: func(t *testing.T, _ *Manager, _ string, beta *Polecat, _ string) {
				dirtyWorktree(t, beta.ClonePath)
				runGit(t, beta.ClonePath, "stash", "push", "-u", "-m", "beta-stash")
			},
		},
		{
			name: "live session",
			setup: func(t *testing.T, mgr *Manager, _ string, _ *Polecat, _ string) {
				tm := newFakeProbe()
				sess := session.PolecatSessionName(session.PrefixFor(mgr.rig.Name), "beta")
				if err := tm.NewSessionWithCommandAndEnv(sess, t.TempDir(), "sleep 300", nil); err != nil {
					t.Fatalf("create session: %v", err)
				}
				mgr.tmux = tm
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr, mayorRig, _, beta, heldBranch, _ := idleHolderFixture(t)
			tc.setup(t, mgr, mayorRig, beta, heldBranch)

			_, err := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: "gt-next", ResumeBranch: heldBranch})
			if !errors.Is(err, ErrBranchHeld) {
				t.Fatalf("want ErrBranchHeld, got %v", err)
			}
			if !strings.Contains(err.Error(), "holder not released") {
				t.Errorf("refusal does not say why the holder was kept: %v", err)
			}
			if head := gitProbeOutput(t, beta.ClonePath, "symbolic-ref", "--short", "HEAD"); head != heldBranch {
				t.Errorf("beta HEAD = %q, want it left on %q", head, heldBranch)
			}
		})
	}
}
