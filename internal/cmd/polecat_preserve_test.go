package cmd

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// gitNow runs git and returns trimmed stdout, failing the test on error.
func gitNow(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// setupPreserveRepo creates a repo with a bare origin and a polecat branch one
// commit ahead of origin/main, unpublished. It returns the repo path, the bare
// remote path, and the branch name.
func setupPreserveRepo(t *testing.T) (repo, remote, branch string) {
	t.Helper()
	dir := t.TempDir()
	remote = filepath.Join(dir, "remote.git")
	repo = filepath.Join(dir, "repo")
	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, "", "init", repo)
	runGitCmd(t, repo, "config", "user.email", "test@example.com")
	runGitCmd(t, repo, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(repo, "README.md"), "base\n")
	runGitCmd(t, repo, "add", "README.md")
	runGitCmd(t, repo, "commit", "-m", "base")
	runGitCmd(t, repo, "branch", "-M", "main")
	runGitCmd(t, repo, "remote", "add", "origin", remote)
	runGitCmd(t, repo, "push", "-u", "origin", "main")

	branch = "polecat/test/gt-yxys+mu745h79"
	runGitCmd(t, repo, "checkout", "-b", branch)
	writeTestFile(t, filepath.Join(repo, "work.txt"), "work\n")
	runGitCmd(t, repo, "add", "work.txt")
	runGitCmd(t, repo, "commit", "-m", "polecat work")
	return repo, remote, branch
}

// The witness's flint nuke: the branch was pushed, then rewritten locally
// (rebase/amend), so origin/<branch> is at a commit the local tip does not
// descend from. The plain push is rejected non-fast-forward and the work exists
// nowhere on origin — the old code printed "remote branch preserved" and
// deleted the branch anyway.
func TestPreserveBranchBeforeNukeFallsBackToSideRefWhenPushRejected(t *testing.T) {
	repo, _, branch := setupPreserveRepo(t)
	g := git.NewGit(repo)

	runGitCmd(t, repo, "push", "origin", branch)
	remoteTip := gitNow(t, repo, "rev-parse", "refs/remotes/origin/"+branch)
	writeTestFile(t, filepath.Join(repo, "work.txt"), "rewritten work\n")
	runGitCmd(t, repo, "commit", "-a", "--amend", "-m", "polecat work (rebased)")
	tip := gitNow(t, repo, "rev-parse", "refs/heads/"+branch)
	if tip == remoteTip {
		t.Fatal("fixture is wrong: local tip should differ from the published tip")
	}

	outcome, err := preserveBranchBeforeNuke(g, branch, "origin")
	if err != nil {
		t.Fatalf("preserveBranchBeforeNuke: %v", err)
	}
	if outcome.AlreadyThere {
		t.Fatal("AlreadyThere = true, want false: this tip was on no remote ref")
	}
	wantRef := "origin/" + branch + "-" + tip[:7]
	if outcome.RemoteRef != wantRef {
		t.Fatalf("RemoteRef = %q, want %q", outcome.RemoteRef, wantRef)
	}
	if got := gitNow(t, repo, "rev-parse", "refs/remotes/"+wantRef); got != tip {
		t.Fatalf("%s = %s, want the preserved tip %s", wantRef, got, tip)
	}
	if got := gitNow(t, repo, "rev-parse", "refs/remotes/origin/"+branch); got != remoteTip {
		t.Fatalf("origin/%s = %s, want it left alone at %s", branch, got, remoteTip)
	}
}

// A branch already merged into main is preserved even though no origin/<branch>
// exists: reachability, not tip membership, is the test. The jade run showed
// two heads that are ancestors of main where `ls-remote` on the branch name
// found nothing.
func TestPreserveBranchBeforeNukeRecognizesWorkAlreadyMergedToMain(t *testing.T) {
	repo, remote, branch := setupPreserveRepo(t)
	g := git.NewGit(repo)

	runGitCmd(t, repo, "checkout", "main")
	runGitCmd(t, repo, "merge", "--ff-only", branch)
	runGitCmd(t, repo, "push", "origin", "main")
	if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); published != "" {
		t.Fatalf("fixture is wrong: origin/%s should not exist, got %q", branch, published)
	}

	outcome, err := preserveBranchBeforeNuke(g, branch, "origin")
	if err != nil {
		t.Fatalf("preserveBranchBeforeNuke: %v", err)
	}
	if !outcome.AlreadyThere {
		t.Fatal("AlreadyThere = false, want true: the tip is an ancestor of origin/main")
	}
	if outcome.RemoteRef != "origin/main" {
		t.Fatalf("RemoteRef = %q, want origin/main", outcome.RemoteRef)
	}
	if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); published != "" {
		t.Fatalf("no push was needed, but origin/%s was created (%q)", branch, published)
	}
}

// With the branch tip reachable only locally and a remote that accepts no push,
// there is nowhere safe for the work to go. The preserve step must say so
// instead of letting the caller delete the branch.
func TestPreserveBranchBeforeNukeFailsClosedWhenNoRemoteRefCanHoldTheTip(t *testing.T) {
	repo, _, branch := setupPreserveRepo(t)
	g := git.NewGit(repo)

	// Fetch keeps working; every push fails.
	runGitCmd(t, repo, "config", "remote.origin.pushurl", filepath.Join(t.TempDir(), "gone.git"))

	_, err := preserveBranchBeforeNuke(g, branch, "origin")
	if !errors.Is(err, errBranchNotPreserved) {
		t.Fatalf("err = %v, want it to wrap errBranchNotPreserved", err)
	}
}

func TestPreserveFailureBlockerRequiresForceAndAcknowledgement(t *testing.T) {
	cause := fmt.Errorf("%w: push rejected", errBranchNotPreserved)

	cases := []struct {
		name        string
		cause       error
		force, ack  bool
		wantBlocked bool
	}{
		{"preserved branch never blocks", nil, false, false, false},
		{"unpreserved, no override", cause, false, false, true},
		{"unpreserved, --force alone", cause, true, false, true},
		{"unpreserved, acknowledgement alone", cause, false, true, true},
		{"unpreserved, --force plus acknowledgement", cause, true, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := preserveFailureBlocker("gastown", "flint", tc.force, tc.ack, tc.cause)
			if (err != nil) != tc.wantBlocked {
				t.Fatalf("blocked = %v (err %v), want %v", err != nil, err, tc.wantBlocked)
			}
			if err == nil {
				return
			}
			// The operator has to be able to see both what happened and how to
			// get out of it, without re-reading the source.
			for _, want := range []string{EnvNukeAcknowledgeUnpreserved, "NOT deleted"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must mention %q", err, want)
				}
			}
		})
	}
}

// The sling-rollback path never pushes, so it must not claim preservation. A
// tip with no remote copy stays local rather than being deleted.
func TestDeletePolecatBranchKeepsLocalBranchWithoutRemoteCopy(t *testing.T) {
	repo, _, branch := setupPreserveRepo(t)
	g := git.NewGit(repo)
	// Production deletes the branch from the bare repo, after the worktree that
	// had it checked out is gone; check out main so git allows the delete.
	runGitCmd(t, repo, "checkout", "main")

	deletePolecatBranch(branch, g, false)

	if out := gitNow(t, repo, "branch", "--list", branch); out == "" {
		t.Fatal("local branch was deleted even though no remote ref contains its tip")
	}
}

func TestDeletePolecatBranchDeletesWhenWorkIsOnRemote(t *testing.T) {
	repo, remote, branch := setupPreserveRepo(t)
	g := git.NewGit(repo)
	runGitCmd(t, repo, "push", "origin", branch)
	runGitCmd(t, repo, "checkout", "main")

	deletePolecatBranch(branch, g, false)

	if out := gitNow(t, repo, "branch", "--list", branch); out != "" {
		t.Fatalf("local branch %s should be deleted once origin holds the work", branch)
	}
	if published := gitNow(t, repo, "ls-remote", "--heads", remote, branch); published == "" {
		t.Fatalf("origin/%s should still hold the work", branch)
	}
}
