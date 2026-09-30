package polecat

import (
	"errors"
	"os"
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

// idleHolderFixture is a rig where beta, an idle polecat, holds heldBranch,
// which origin also has at main's commit. alpha is the polecat that resumes it.
func idleHolderFixture(t *testing.T) (mgr *Manager, w *world, alpha, beta *Polecat, heldBranch, tip string) {
	t.Helper()
	mgr, mayorRig, _, added, w := canonicalWithPolecats(t, false, "alpha", "beta")
	alpha, beta = added["alpha"], added["beta"]

	tip = w.rev(t, mayorRig, "origin/main")
	heldBranch = "polecat/beta/gt-x+aaa"
	w.SetRef(t, mayorRig, "refs/heads/"+heldBranch, tip)
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+heldBranch, tip)
	w.switchTo(t, beta.ClonePath, heldBranch)
	return mgr, w, alpha, beta, heldBranch, tip
}

func TestReuseIdlePolecat_ResumesBranchHeldByIdleUnreapedPolecat(t *testing.T) {
	t.Parallel()
	mgr, w, alpha, beta, heldBranch, tip := idleHolderFixture(t)

	reused, err := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: "gt-next", ResumeBranch: heldBranch})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat refused a branch held by an idle polecat: %v", err)
	}
	if reused.ClonePath != alpha.ClonePath {
		t.Errorf("reused %s, want alpha's worktree %s", reused.ClonePath, alpha.ClonePath)
	}
	if head := w.branch(t, alpha.ClonePath); head != heldBranch {
		t.Errorf("alpha HEAD = %q, want %q", head, heldBranch)
	}

	// beta gave up the checkout, not the branch, and stays a reusable slot.
	if b := w.branch(t, beta.ClonePath); b != "HEAD" {
		t.Errorf("beta is still on a branch (%s), want a detached HEAD", b)
	}
	if got := w.rev(t, beta.ClonePath, "HEAD"); got != tip {
		t.Errorf("beta HEAD = %s, want %s", got, tip)
	}
	if got := w.rev(t, alpha.ClonePath, "refs/heads/"+heldBranch); got != tip {
		t.Errorf("refs/heads/%s = %s, want %s", heldBranch, got, tip)
	}
	if decision := mgr.ReuseDecisionForPolecat("beta", StateIdle); !decision.Reusable {
		t.Errorf("beta is not reusable after release: %s", decision.Reason)
	}
}

func TestAddWithOptions_ResumesBranchHeldByIdleUnreapedPolecat(t *testing.T) {
	t.Parallel()
	mgr, w, _, beta, heldBranch, _ := idleHolderFixture(t)

	gamma, err := mgr.AddWithOptions("gamma", AddOptions{HookBead: "gt-next", ResumeBranch: heldBranch})
	if err != nil {
		t.Fatalf("AddWithOptions refused a branch held by an idle polecat: %v", err)
	}
	if head := w.branch(t, gamma.ClonePath); head != heldBranch {
		t.Errorf("gamma HEAD = %q, want %q", head, heldBranch)
	}
	if w.branch(t, beta.ClonePath) != "HEAD" {
		t.Error("beta is still on the branch gamma resumed")
	}
}

// TestReuseIdlePolecat_KeepsRefusingHolderWithSomethingToLose covers each way a
// holder stops being an idle slot: the release must leave it untouched.
func TestReuseIdlePolecat_KeepsRefusingHolderWithSomethingToLose(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(t *testing.T, w *world, mgr *Manager, beta *Polecat)
	}{
		{
			name: "uncommitted edits",
			setup: func(t *testing.T, w *world, _ *Manager, beta *Polecat) {
				dirtyWorktree(t, beta.ClonePath)
			},
		},
		{
			name: "stashed work",
			setup: func(t *testing.T, w *world, _ *Manager, beta *Polecat) {
				dirtyWorktree(t, beta.ClonePath)
				w.Stash(t, beta.ClonePath, "beta-stash")
			},
		},
		{
			name: "live session",
			setup: func(t *testing.T, _ *world, mgr *Manager, _ *Polecat) {
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
			mgr, w, _, beta, heldBranch, _ := idleHolderFixture(t)
			tc.setup(t, w, mgr, beta)

			_, err := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: "gt-next", ResumeBranch: heldBranch})
			if !errors.Is(err, ErrBranchHeld) {
				t.Fatalf("want ErrBranchHeld, got %v", err)
			}
			if !strings.Contains(err.Error(), "holder not released") {
				t.Errorf("refusal does not say why the holder was kept: %v", err)
			}
			if head := w.branch(t, beta.ClonePath); head != heldBranch {
				t.Errorf("beta HEAD = %q, want it left on %q", head, heldBranch)
			}
		})
	}
}
