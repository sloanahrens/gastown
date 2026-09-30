package polecat

import (
	"errors"
	"strings"
	"testing"

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
// hooked bead's labels, as a JSON array body.
func stalledHolderFixture(t *testing.T, labels string) (mgr *Manager, alpha, beta *Polecat, heldBranch, tip string) {
	t.Helper()
	mgr, mayorRig, bd, added := setupCanonicalWithPolecats(t, false, "alpha", "beta")
	alpha, beta = added["alpha"], added["beta"]

	tip = gitProbeOutput(t, mayorRig, "rev-parse", "origin/main")
	heldBranch = "polecat/beta/gt-x+aaa"
	runGit(t, mayorRig, "update-ref", "refs/heads/"+heldBranch, tip)
	runGit(t, mayorRig, "update-ref", "refs/remotes/origin/"+heldBranch, tip)
	runGit(t, beta.ClonePath, "checkout", heldBranch)

	// A fake tmux with no session: the session is provably down. Without one,
	// the manager cannot tell a dead session from a live one.
	mgr.tmux = newFakeProbe()

	base := bd.answer
	hooked := `[{"id":"` + stalledBead + `","status":"hooked","assignee":"` + mgr.assigneeID("beta") + `","labels":` + labels + `}]`
	bd.mu.Lock()
	bd.answer = func(cmd string, args []string) string {
		if cmd == "list" && strings.Contains(strings.Join(args, " "), "hooked") &&
			strings.Contains(strings.Join(args, " "), mgr.assigneeID("beta")) {
			return hooked
		}
		return base(cmd, args)
	}
	bd.mu.Unlock()
	return mgr, alpha, beta, heldBranch, tip
}

func TestReuseIdlePolecat_ResumesBranchHeldByStalledPolecatOnSameBead(t *testing.T) {
	t.Parallel()
	mgr, alpha, beta, heldBranch, tip := stalledHolderFixture(t, `[]`)

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
	if head := gitProbeOutput(t, alpha.ClonePath, "symbolic-ref", "--short", "HEAD"); head != heldBranch {
		t.Errorf("alpha HEAD = %q, want %q", head, heldBranch)
	}

	// beta gave up the checkout, not the branch.
	if out, err := runGitOutput(beta.ClonePath, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Errorf("beta is still on a branch (%s), want a detached HEAD", out)
	}
	if got := gitProbeOutput(t, alpha.ClonePath, "rev-parse", "refs/heads/"+heldBranch); got != tip {
		t.Errorf("refs/heads/%s = %s, want %s", heldBranch, got, tip)
	}
}

func TestAddWithOptions_ResumesBranchHeldByStalledPolecatOnSameBead(t *testing.T) {
	t.Parallel()
	mgr, _, beta, heldBranch, _ := stalledHolderFixture(t, `[]`)

	gamma, err := mgr.AddWithOptions("gamma", AddOptions{HookBead: stalledBead, ResumeBranch: heldBranch})
	if err != nil {
		t.Fatalf("AddWithOptions refused a branch held by a stalled polecat on the same bead: %v", err)
	}
	if head := gitProbeOutput(t, gamma.ClonePath, "symbolic-ref", "--short", "HEAD"); head != heldBranch {
		t.Errorf("gamma HEAD = %q, want %q", head, heldBranch)
	}
	if _, err := runGitOutput(beta.ClonePath, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Error("beta is still on the branch gamma resumed")
	}
}

// TestReuseIdlePolecat_KeepsRefusingStalledHolderItMustNotRelease covers each
// way a stalled holder stays put: the release must leave it untouched.
func TestReuseIdlePolecat_KeepsRefusingStalledHolderItMustNotRelease(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		labels string
		bead   string // the bead alpha is slung; stalledBead when empty
		setup  func(t *testing.T, mgr *Manager, beta *Polecat)
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
			labels: `["gt:ready-to-land"]`,
		},
		{
			name: "uncommitted edits",
			setup: func(t *testing.T, _ *Manager, beta *Polecat) {
				dirtyWorktree(t, beta.ClonePath)
			},
		},
		{
			name: "stashed work",
			setup: func(t *testing.T, _ *Manager, beta *Polecat) {
				dirtyWorktree(t, beta.ClonePath)
				runGit(t, beta.ClonePath, "stash", "push", "-u", "-m", "beta-stash")
			},
		},
		{
			name: "a commit origin does not have",
			setup: func(t *testing.T, _ *Manager, beta *Polecat) {
				// The fixture's origin is the rig's own repo, which the polecat
				// worktrees share, so a new commit would already be "on origin".
				// Give origin a repo of its own that has the branch at its old tip.
				bare := t.TempDir()
				runGit(t, beta.ClonePath, "clone", "--bare", "--no-local", ".", bare)
				runGit(t, beta.ClonePath, "remote", "set-url", "origin", bare)
				dirtyWorktree(t, beta.ClonePath)
				runGit(t, beta.ClonePath, "-c", "user.name=t", "-c", "user.email=t@example.com",
					"commit", "-a", "-m", "unpushed")
			},
		},
		{
			name: "live session",
			setup: func(t *testing.T, mgr *Manager, _ *Polecat) {
				sess := session.PolecatSessionName(session.PrefixFor(mgr.rig.Name), "beta")
				if err := mgr.tmux.(*fakeProbe).NewSessionWithCommandAndEnv(sess, t.TempDir(), "sleep 300", nil); err != nil {
					t.Fatalf("create session: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			labels := tc.labels
			if labels == "" {
				labels = `[]`
			}
			mgr, _, beta, heldBranch, _ := stalledHolderFixture(t, labels)
			if tc.setup != nil {
				tc.setup(t, mgr, beta)
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
			if head := gitProbeOutput(t, beta.ClonePath, "symbolic-ref", "--short", "HEAD"); head != heldBranch {
				t.Errorf("beta HEAD = %q, want it left on %q", head, heldBranch)
			}
		})
	}
}
