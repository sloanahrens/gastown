package polecat

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/session"
)

// gt-70m1: a resume must not be refused because the branch's author polecat
// died mid-work. Its session is gone and its bead is still hooked, so it reads
// stalled, not idle. It is released only for the bead it holds, and only when
// nothing on its disk is at risk.

const stalledBead = "gt-x"

// stalledHolderFixture is a rig where beta, a polecat whose session is dead and
// whose bead stalledBead is still hooked, holds heldBranch, which origin also
// has at main's commit. alpha is the polecat that resumes it. labels are the
// hooked bead's labels.
func stalledHolderFixture(t *testing.T, labels []string) (mgr *Manager, w *world, alpha, beta *Polecat, heldBranch, tip string) {
	t.Helper()
	mgr, mayorRig, bd, added, w := canonicalWithPolecats(t, false, "alpha", "beta")
	alpha, beta = added["alpha"], added["beta"]

	tip = w.rev(t, mayorRig, "origin/main")
	heldBranch = "polecat/beta/gt-x+aaa"
	w.SetRef(t, mayorRig, "refs/heads/"+heldBranch, tip)
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+heldBranch, tip)
	w.switchTo(t, beta.ClonePath, heldBranch)

	// A fake tmux with no session: the session is provably down. Without one,
	// the manager cannot tell a dead session from a live one.
	mgr.tmux = newFakeProbe()

	bd.Seed(beads.Issue{ID: stalledBead, Title: "work", Status: "hooked", Assignee: mgr.assigneeID("beta"), Labels: labels})
	return mgr, w, alpha, beta, heldBranch, tip
}

func TestReuseIdlePolecat_ResumesBranchHeldByStalledPolecatOnSameBead(t *testing.T) {
	t.Parallel()
	mgr, w, alpha, beta, heldBranch, tip := stalledHolderFixture(t, nil)

	if p, err := mgr.loadFromBeads("beta", nil); err != nil || p.State != StateStalled {
		t.Fatalf("fixture: beta = %+v, %v; want a stalled polecat", p, err)
	}

	reused, err := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: stalledBead, ResumeBranch: heldBranch})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat refused a branch held by a stalled polecat on the same bead: %v", err)
	}
	if reused.ClonePath != alpha.ClonePath {
		t.Errorf("reused %s, want alpha's worktree %s", reused.ClonePath, alpha.ClonePath)
	}
	if head := w.branch(t, alpha.ClonePath); head != heldBranch {
		t.Errorf("alpha HEAD = %q, want %q", head, heldBranch)
	}

	// beta gave up the checkout, not the branch.
	if b := w.branch(t, beta.ClonePath); b != "HEAD" {
		t.Errorf("beta is still on a branch (%s), want a detached HEAD", b)
	}
	if got := w.rev(t, alpha.ClonePath, "refs/heads/"+heldBranch); got != tip {
		t.Errorf("refs/heads/%s = %s, want %s", heldBranch, got, tip)
	}
}

func TestAddWithOptions_ResumesBranchHeldByStalledPolecatOnSameBead(t *testing.T) {
	t.Parallel()
	mgr, w, _, beta, heldBranch, _ := stalledHolderFixture(t, nil)

	gamma, err := mgr.AddWithOptions("gamma", AddOptions{HookBead: stalledBead, ResumeBranch: heldBranch})
	if err != nil {
		t.Fatalf("AddWithOptions refused a branch held by a stalled polecat on the same bead: %v", err)
	}
	if head := w.branch(t, gamma.ClonePath); head != heldBranch {
		t.Errorf("gamma HEAD = %q, want %q", head, heldBranch)
	}
	if w.branch(t, beta.ClonePath) != "HEAD" {
		t.Error("beta is still on the branch gamma resumed")
	}
}

// TestReuseIdlePolecat_KeepsRefusingStalledHolderItMustNotRelease covers each
// way a stalled holder stays put: the release must leave it untouched.
func TestReuseIdlePolecat_KeepsRefusingStalledHolderItMustNotRelease(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		labels []string
		bead   string // the bead alpha is slung; stalledBead when empty
		setup  func(t *testing.T, w *world, mgr *Manager, beta *Polecat)
	}{
		{
			name: "a different bead than the one it holds",
			bead: "gt-other",
		},
		{
			name: "no bead named",
			bead: "-",
		},
		{
			name:   "submitted for landing",
			labels: []string{"gt:ready-to-land"},
		},
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
			name: "a commit origin does not have",
			setup: func(t *testing.T, w *world, _ *Manager, beta *Polecat) {
				// The fixture's origin is the rig's own repo, which the polecat
				// worktrees share, so a new commit would already be "on origin".
				// Give origin a repo of its own that has the branch at its old tip.
				bare := filepath.Join(t.TempDir(), "origin.git")
				w.InitBare(t, bare)
				tip := w.rev(t, beta.ClonePath, "HEAD")
				branch := w.branch(t, beta.ClonePath)
				w.SetRef(t, bare, "refs/heads/"+branch, tip)
				w.SetRef(t, bare, "refs/heads/main", tip)
				w.AddRemote(t, beta.ClonePath, "origin", bare)
				dirtyWorktree(t, beta.ClonePath)
				w.CommitWorktree(t, beta.ClonePath, "unpushed")
			},
		},
		{
			name: "live session",
			setup: func(t *testing.T, _ *world, mgr *Manager, _ *Polecat) {
				sess := session.PolecatSessionName(session.DefaultPrefix, "beta")
				if err := mgr.tmux.(*fakeProbe).NewSessionWithCommandAndEnv(sess, t.TempDir(), "sleep 300", nil); err != nil {
					t.Fatalf("create session: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr, w, _, beta, heldBranch, _ := stalledHolderFixture(t, tc.labels)
			if tc.setup != nil {
				tc.setup(t, w, mgr, beta)
			}
			bead := tc.bead
			switch bead {
			case "":
				bead = stalledBead
			case "-":
				bead = ""
			}

			_, err := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: bead, ResumeBranch: heldBranch})
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
