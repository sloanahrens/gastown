package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

func TestSandboxSeatGitStateRunningSandboxIsUnknown(t *testing.T) {
	t.Parallel()
	st := SandboxSeatGitState("polecat/x/gt-1", true, nil, "ma-papaya")
	if st.Source != GitStateSourceUnknown {
		t.Fatalf("Source = %q, want unknown: the work lives in the running sandbox", st.Source)
	}
	if !strings.Contains(st.FailedReason, "ma-papaya") {
		t.Errorf("FailedReason = %q, want it to name the sandbox", st.FailedReason)
	}
}

func TestSandboxSeatGitStateGoneSandboxIsCleanAndLive(t *testing.T) {
	t.Parallel()
	st := SandboxSeatGitState("polecat/x/gt-1", false, nil, "ma-papaya")
	if st.Source != GitStateSourceLive {
		t.Errorf("Source = %q, want live: a gone sandbox has nothing left to measure", st.Source)
	}
	if st.Branch != "polecat/x/gt-1" {
		t.Errorf("Branch = %q, want the host branch label", st.Branch)
	}
	if st.FailedReason != "" {
		t.Errorf("FailedReason = %q, want none on a live answer", st.FailedReason)
	}
	if st.Dirty || st.StashCount != 0 || st.UnpushedCommits != 0 {
		t.Errorf("host worktree facts leaked into a sandbox answer: Dirty=%v StashCount=%d UnpushedCommits=%d",
			st.Dirty, st.StashCount, st.UnpushedCommits)
	}
}

// A gateway that errors (down, timed out) is not an answer: a live sandbox must
// not read as gone, so the state stays unknown whatever exists says.
func TestSandboxSeatGitStateGatewayErrorIsUnknown(t *testing.T) {
	t.Parallel()
	for _, exists := range []bool{false, true} {
		st := SandboxSeatGitState("polecat/x/gt-1", exists, errors.New("gateway unreachable"), "ma-papaya")
		if st.Source != GitStateSourceUnknown {
			t.Errorf("exists=%v with a gateway error: Source = %v, want unknown", exists, st.Source)
		}
		if !strings.Contains(st.FailedReason, "gateway unreachable") {
			t.Errorf("exists=%v: FailedReason = %q, want the gateway error in it", exists, st.FailedReason)
		}
	}
}

// Only a gone seat is measured: a running sandbox and a gateway error keep the
// answer they had, whatever host worktree the caller names.
func TestSandboxSeatHostGitStateLeavesEveryOtherAnswerAlone(t *testing.T) {
	t.Parallel()
	for _, live := range []LiveGitState{
		SandboxSeatGitState("label", true, nil, "sb"),
		SandboxSeatGitState("", false, errors.New("gateway unreachable"), "sb"),
	} {
		got := SandboxSeatHostGitState(live, t.TempDir(), "", "polecat/toast/gt-a+x")
		if got != live {
			t.Errorf("state %+v was rewritten to %+v", live, got)
		}
	}
}

// sandboxHostRepo is a clone of an origin with main, checked out on a polecat
// branch that carries one commit pushed to origin.
func sandboxHostRepo(t *testing.T) (*world, string, string) {
	t.Helper()
	w, dir := initLiveGitRepo(t)
	const branch = "polecat/toast/gt-a+x"
	if err := w.repo(dir).CheckoutNewBranch(branch, "HEAD"); err != nil {
		t.Fatalf("checkout %s: %v", branch, err)
	}
	w.writeAndCommit(t, dir, "the sandbox's fix", map[string]string{"fix.txt": "x\n"})
	if err := w.repo(dir).Push("origin", branch, false); err != nil {
		t.Fatalf("push %s: %v", branch, err)
	}
	return w, dir, branch
}

// judgeGone measures a gone sandbox seat's host worktree in w.
func judgeGone(w *world, worktreePath, pushedBranch string) LiveGitState {
	return judgeGoneIn(w, worktreePath, "", pushedBranch)
}

// judgeGoneIn is judgeGone with the seat's rig root, which holds the shared
// repository a missing worktree is judged from.
func judgeGoneIn(w *world, worktreePath, rigRoot, pushedBranch string) LiveGitState {
	return sandboxSeatHostGitState(w.opener(), SandboxSeatGitState("", false, nil, "sb"), worktreePath, rigRoot, pushedBranch)
}

// A gone sandbox seat is judged from its host worktree: commits whose patches
// are already on the pushed branch are not unique work, a unique commit is
// counted, and every way the comparison can fail is unknown rather than clean
// (gt-zgoo3).
func TestSandboxSeatGitStateGoneSandboxJudgesTheHostWorktree(t *testing.T) {
	t.Parallel()

	t.Run("commits on the pushed branch are clean", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		st := judgeGone(w, dir, branch)
		if st.Source != GitStateSourceLive {
			t.Fatalf("Source = %q, want live (reason %q)", st.Source, st.FailedReason)
		}
		if st.Dirty || st.StashCount != 0 || st.UnpushedCommits != 0 {
			t.Errorf("host worktree with nothing unique read as dirty=%v stash=%d unpushed=%d", st.Dirty, st.StashCount, st.UnpushedCommits)
		}
	})

	t.Run("a unique commit is counted", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		if err := w.repo(dir).CheckoutResetBranch(branch, "main"); err != nil {
			t.Fatalf("rewind %s: %v", branch, err)
		}
		w.writeAndCommit(t, dir, "only the host has this", map[string]string{"host.txt": "y\n"})
		st := judgeGone(w, dir, branch)
		if st.Source != GitStateSourceLive {
			t.Fatalf("Source = %q, want live (reason %q)", st.Source, st.FailedReason)
		}
		if st.UnpushedCommits != 1 {
			t.Errorf("UnpushedCommits = %d, want 1 (the host-only commit)", st.UnpushedCommits)
		}
	})

	t.Run("dirt is reported", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "churn.txt"), []byte("churn\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		st := judgeGone(w, dir, branch)
		if st.Source != GitStateSourceLive || !st.Dirty {
			t.Errorf("state = %+v, want a live dirty answer", st)
		}
	})

	t.Run("a stash is reported", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		w.Stash(t, dir, "wip")
		st := judgeGone(w, dir, branch)
		if st.Source != GitStateSourceLive || st.StashCount != 1 {
			t.Errorf("state = %+v, want a live answer naming the one stash", st)
		}
	})

	t.Run("an unresolved worktree path is unknown", func(t *testing.T) {
		t.Parallel()
		st := judgeGone(newWorld(), "", "polecat/toast/gt-a+x")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "no host worktree path") {
			t.Errorf("state = %+v, want unknown naming the missing path", st)
		}
	})

	t.Run("a missing worktree with no shared repository is unknown", func(t *testing.T) {
		t.Parallel()
		st := judgeGoneIn(newWorld(), filepath.Join(t.TempDir(), "gone"), t.TempDir(), "polecat/toast/gt-a+x")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "locating the shared repository") {
			t.Errorf("state = %+v, want unknown naming the shared repository", st)
		}
	})

	t.Run("a worktree path under a file is unknown", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		st := judgeGone(newWorld(), filepath.Join(file, "work"), "polecat/toast/gt-a+x")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "reading the host worktree") {
			t.Errorf("state = %+v, want unknown naming the unreadable worktree", st)
		}
	})

	t.Run("a directory that is not a worktree root is unknown", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		inner := filepath.Join(dir, "sub")
		if err := os.Mkdir(inner, 0o755); err != nil {
			t.Fatal(err)
		}
		st := judgeGone(w, inner, branch)
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "worktree root") {
			t.Errorf("state = %+v, want unknown naming the worktree root", st)
		}
	})

	t.Run("an unreadable branch is unknown", func(t *testing.T) {
		t.Parallel()
		w := newWorld()
		dir := filepath.Join(t.TempDir(), "unborn")
		w.InitRepo(t, dir)
		st := judgeGone(w, dir, "polecat/toast/gt-a+x")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "reading branch") {
			t.Errorf("state = %+v, want unknown naming the branch read", st)
		}
	})

	t.Run("a pruned branch falls back to the default branch", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		// The branch landed: the default branch carries the same work, and the
		// seat's branch is gone from this clone's view of origin.
		w.SetRef(t, dir, "refs/remotes/origin/main", w.rev(t, dir, "refs/heads/"+branch))
		w.DeleteRef(t, dir, "refs/remotes/origin/"+branch)
		st := judgeGone(w, dir, branch)
		if st.Source != GitStateSourceLive || st.UnpushedCommits != 0 {
			t.Errorf("state = %+v, want a clean answer from the default branch", st)
		}
	})

	t.Run("the default-branch fallback still counts unique work", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		w.DeleteRef(t, dir, "refs/remotes/origin/"+branch)
		st := judgeGone(w, dir, branch)
		if st.Source != GitStateSourceLive || st.UnpushedCommits != 1 {
			t.Errorf("state = %+v, want the one unique commit counted", st)
		}
	})

	t.Run("no default branch to compare against is unknown", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		w.DeleteRef(t, dir, "refs/remotes/origin/"+branch)
		w.DeleteRef(t, dir, "refs/remotes/origin/main")
		st := judgeGone(w, dir, branch)
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "comparing HEAD with") {
			t.Errorf("state = %+v, want unknown naming the comparison", st)
		}
	})

	t.Run("an unreadable comparison is unknown", func(t *testing.T) {
		t.Parallel()
		w := newWorld()
		dir := filepath.Join(t.TempDir(), "work")
		w.InitRepo(t, dir)
		w.Commit(t, dir, "main", "initial", map[string]string{"README.md": "# Test\n"})
		w.checkout(t, dir)
		st := judgeGone(w, dir, "polecat/toast/gt-a+x")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "comparing HEAD with") {
			t.Errorf("state = %+v, want unknown naming the comparison", st)
		}
	})

	t.Run("a recorded branch that is not a ref name is unknown", func(t *testing.T) {
		t.Parallel()
		w, dir, _ := sandboxHostRepo(t)
		for _, recorded := range []string{"--upload-pack=evil", "HEAD", "bad..name", "-x"} {
			st := judgeGone(w, dir, recorded)
			if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "not a valid ref name") {
				t.Errorf("recorded branch %q judged %+v, want unknown naming the record", recorded, st)
			}
		}
	})

	t.Run("a shared repo branch holding a commit the worktree lacks is counted", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		rigRoot := t.TempDir()
		buildRigIn(t, w, rigRoot, map[string]string{"README.md": "# Test\n"})
		shared := constants.RigBareRepoPath(rigRoot)
		// The shared repository kept the seat's branch at a commit the worktree
		// never reached.
		w.SetRef(t, shared, "refs/heads/"+branch, w.rev(t, shared, "refs/heads/main"))
		w.Commit(t, shared, branch, "only the shared ref has this", map[string]string{"shared.txt": "s\n"})
		st := judgeGoneIn(w, dir, rigRoot, branch)
		if st.Source != GitStateSourceLive || st.UnpushedCommits != 1 {
			t.Errorf("state = %+v, want the shared ref's one unique commit counted", st)
		}
	})

	t.Run("an unreadable shared repo comparison in a live worktree is unknown", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		rigRoot := t.TempDir()
		buildRigIn(t, w, rigRoot, map[string]string{"README.md": "# Test\n"})
		shared := constants.RigBareRepoPath(rigRoot)
		w.SetRef(t, shared, "refs/heads/"+branch, w.rev(t, shared, "refs/heads/main"))
		w.DeleteRef(t, shared, "refs/remotes/origin/main")
		st := judgeGoneIn(w, dir, rigRoot, branch)
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "judging refs/heads/") {
			t.Errorf("state = %+v, want unknown naming the shared ref comparison", st)
		}
	})

	t.Run("an unreadable shared repo ref in a live worktree is unknown", func(t *testing.T) {
		t.Parallel()
		w, dir, branch := sandboxHostRepo(t)
		rigRoot := t.TempDir()
		// A rig root with a legacy mayor/rig clone and no .repo.git: the shared repository cannot be read.
		if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
			t.Fatal(err)
		}
		st := judgeGoneIn(w, dir, rigRoot, branch)
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "reading refs/heads/"+branch+" in the shared repository") {
			t.Errorf("state = %+v, want unknown naming the shared ref read", st)
		}
	})
}

// A gone sandbox seat's recorded branch is read from the rig's shared .repo.git,
// the second copy of the seat's work. A rig with no .repo.git, whether it has
// neither repo or only a legacy mayor/rig clone, cannot prove that copy is
// clean, so the seat is unknown, never clean, through both readers that open
// the shared repository: a present host worktree and a gone one (gt-3lvcjz.4).
func TestLiveGitRigWithNoSharedRepoIsUnknownForASandboxSeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		legacyClone bool
	}{
		{name: "a rig with neither repo is unknown", legacyClone: false},
		{name: "a rig with only a mayor/rig clone is unknown", legacyClone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, hostDir, branch := sandboxHostRepo(t)
			rigRoot := t.TempDir()
			if tc.legacyClone {
				if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			goneDir := filepath.Join(rigRoot, "polecats", "toast", "gastown")
			for _, seat := range []struct {
				name string
				path string
			}{
				{name: "host worktree present", path: hostDir},
				{name: "host worktree gone", path: goneDir},
			} {
				t.Run(seat.name, func(t *testing.T) {
					t.Parallel()
					st := judgeGoneIn(w, seat.path, rigRoot, branch)
					want := "no .repo.git under " + rigRoot
					if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, want) {
						t.Errorf("state = %+v, want unknown naming %q", st, want)
					}
				})
			}
		})
	}
}

// sharedRepoSeatRef fails closed for a rig root with no .repo.git, with or
// without a legacy mayor/rig clone: the answer is ErrNoBareRepo, never the
// not-found of a repository that holds no ref. An empty rig root names no
// repository, so it is not found.
func TestSharedRepoSeatRefWithoutBareRepo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		legacy  bool // a mayor/rig clone with no .repo.git beside it
		empty   bool // the caller names no rig root
		wantErr bool
	}{
		{name: "a rig with no git state fails closed", wantErr: true},
		{name: "a rig with only a mayor/rig clone fails closed", legacy: true, wantErr: true},
		{name: "an empty rig root is not found", empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.empty {
				root = " "
			}
			if tc.legacy {
				if err := os.MkdirAll(filepath.Join(root, "mayor", "rig"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			g, ref, ok, err := sharedRepoSeatRef(gitOpener{}, root, "polecat/toast/gt-a+x")
			if !tc.wantErr {
				if err != nil || ok || g != nil || ref != "" {
					t.Fatalf("sharedRepoSeatRef = (%v, %q, %v, %v), want not found with no error", g, ref, ok, err)
				}
				return
			}
			if !errors.Is(err, ErrNoBareRepo) || !strings.Contains(err.Error(), "no .repo.git under "+root) {
				t.Fatalf("err = %v, want ErrNoBareRepo naming no .repo.git under %s", err, root)
			}
			if ok {
				t.Fatalf("ok = true for a rig with no .repo.git, want false")
			}
		})
	}
}

const sharedRepoBranch = "polecat/toast/gt-a+x"

// sharedRepoRig builds a rig whose shared .repo.git holds the
// seat's branch with one commit, and returns the world, the rig root, and the
// path the seat's gone worktree directory is named by.
func sharedRepoRig(t *testing.T) (*world, string, string) {
	t.Helper()
	w := newWorld()
	rigRoot := t.TempDir()
	buildRigIn(t, w, rigRoot, map[string]string{"README.md": "# Test\n"})
	shared := constants.RigBareRepoPath(rigRoot)
	w.SetRef(t, shared, "refs/heads/"+sharedRepoBranch, w.rev(t, shared, "refs/heads/main"))
	w.Commit(t, shared, sharedRepoBranch, "the seat's commit", map[string]string{"fix.txt": "x\n"})
	return w, rigRoot, filepath.Join(rigRoot, "polecats", "toast", "gastown")
}

// A gone sandbox seat whose host worktree directory is missing is judged from
// the branch refs that worktree left in the rig's shared repository: a commit
// only a ref holds blocks and is counted, a ref whose work is on the pushed
// branch — or no ref at all — reads clean, and a comparison that cannot run is
// unknown. A seat that recorded no branch is judged by every generated branch
// the repository holds under its name, so its unpushed work cannot go unlooked
// for; without this, unpushed commits outlive the worktree that held them
// (gt-zgoo3).
func TestSandboxSeatGoneSeatMissingWorktreeJudgesTheSharedRepo(t *testing.T) {
	t.Parallel()

	t.Run("an unpushed local branch is counted", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, gone := sharedRepoRig(t)
		st := judgeGoneIn(w, gone, rigRoot, sharedRepoBranch)
		if st.Source != GitStateSourceLive {
			t.Fatalf("Source = %q, want live (reason %q)", st.Source, st.FailedReason)
		}
		if st.UnpushedCommits != 1 {
			t.Errorf("UnpushedCommits = %d, want the local branch's one unique commit", st.UnpushedCommits)
		}
	})

	t.Run("a local branch already pushed reads clean", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, gone := sharedRepoRig(t)
		shared := constants.RigBareRepoPath(rigRoot)
		pushed := w.rev(t, shared, "refs/heads/"+sharedRepoBranch)
		mainTip := w.rev(t, shared, "refs/heads/main")
		w.SetRef(t, shared, "refs/remotes/origin/"+sharedRepoBranch, pushed)
		// The host's copy is the same patch under a different SHA, so only
		// patch identity sees that nothing is unique to the shared repo.
		w.SetRef(t, shared, "refs/heads/"+sharedRepoBranch, mainTip)
		w.Commit(t, shared, sharedRepoBranch, "the host's copy of the fix", map[string]string{"fix.txt": "x\n"})

		st := judgeGoneIn(w, gone, rigRoot, sharedRepoBranch)
		if st.Source != GitStateSourceLive || st.UnpushedCommits != 0 {
			t.Errorf("state = %+v, want a live clean answer from patch identity", st)
		}
	})

	t.Run("no local branch reads clean", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, gone := sharedRepoRig(t)
		w.DeleteRef(t, constants.RigBareRepoPath(rigRoot), "refs/heads/"+sharedRepoBranch)
		st := judgeGoneIn(w, gone, rigRoot, sharedRepoBranch)
		if st.Source != GitStateSourceLive || st.UnpushedCommits != 0 {
			t.Errorf("state = %+v, want live clean for a branch the shared repo never held", st)
		}
	})

	t.Run("an unreadable comparison is unknown", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, gone := sharedRepoRig(t)
		w.DeleteRef(t, constants.RigBareRepoPath(rigRoot), "refs/remotes/origin/main")
		st := judgeGoneIn(w, gone, rigRoot, sharedRepoBranch)
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "comparing refs/heads/") {
			t.Errorf("state = %+v, want unknown naming the comparison", st)
		}
	})

	t.Run("a recorded branch that is not a ref name is unknown", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, gone := sharedRepoRig(t)
		st := judgeGoneIn(w, gone, rigRoot, "--all")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "not a valid ref name") {
			t.Errorf("state = %+v, want unknown naming the record", st)
		}
	})

	t.Run("no recorded branch judges the seat's branches in the shared repo", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, gone := sharedRepoRig(t)
		st := judgeGoneIn(w, gone, rigRoot, "")
		if st.Source != GitStateSourceLive {
			t.Fatalf("Source = %q, want live (reason %q)", st.Source, st.FailedReason)
		}
		if st.UnpushedCommits != 1 {
			t.Errorf("UnpushedCommits = %d, want the seat's one local commit", st.UnpushedCommits)
		}
	})

	t.Run("no recorded branch and no local branch for the seat reads clean", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, gone := sharedRepoRig(t)
		shared := constants.RigBareRepoPath(rigRoot)
		w.DeleteRef(t, shared, "refs/heads/"+sharedRepoBranch)
		// Another seat's branch in the repository is not this seat's work.
		w.SetRef(t, shared, "refs/heads/polecat/other/gt-b+x", w.rev(t, shared, "refs/heads/main"))
		st := judgeGoneIn(w, gone, rigRoot, "")
		if st.Source != GitStateSourceLive || st.UnpushedCommits != 0 {
			t.Errorf("state = %+v, want live clean for a seat with no local branch", st)
		}
	})

	t.Run("no recorded branch with no seat named by the path is unknown", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, _ := sharedRepoRig(t)
		st := judgeGoneIn(w, filepath.Join(t.TempDir(), "elsewhere"), rigRoot, "")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "names no polecat under polecats/") {
			t.Errorf("state = %+v, want unknown naming the unnamed seat", st)
		}
	})

	t.Run("no recorded branch with an unreadable branch list is unknown", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.MkdirAll(constants.RigBareRepoPath(root), 0o755); err != nil {
			t.Fatal(err)
		}
		st := judgeGoneIn(newWorld(), filepath.Join(root, "polecats", "toast", "gastown"), root, "")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "listing the shared repository's branches") {
			t.Errorf("state = %+v, want unknown naming the branch list", st)
		}
	})

	t.Run("no recorded branch with an unreadable comparison is unknown", func(t *testing.T) {
		t.Parallel()
		w, rigRoot, gone := sharedRepoRig(t)
		w.DeleteRef(t, constants.RigBareRepoPath(rigRoot), "refs/remotes/origin/main")
		st := judgeGoneIn(w, gone, rigRoot, "")
		if st.Source != GitStateSourceUnknown || !strings.Contains(st.FailedReason, "judging refs/heads/") {
			t.Errorf("state = %+v, want unknown naming the comparison", st)
		}
	})
}
