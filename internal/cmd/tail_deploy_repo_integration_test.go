//go:build integration

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/version"
)

// tailRepoGit runs git in dir with a fixed identity and returns its trimmed
// output. A git failure fails the test with the call's output.
func tailRepoGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// tailRepoRig makes a town whose gastown rig has the rig's bare repo, with main
// at binary, and a source checkout at mayor/rig cloned at binary. The bare repo
// then moves on to a second and third commit, which only it holds: the stale
// checkout is what a pod's unfetched mayor/rig looks like. It returns the town
// root, the binary's commit, the second commit and main's tip.
func tailRepoRig(t *testing.T) (townRoot, binary, second, tip string) {
	t.Helper()
	townRoot = t.TempDir()
	bare := filepath.Join(townRoot, "gastown", ".repo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	tailRepoGit(t, townRoot, "init", "-q", "--bare", bare)
	tailRepoGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

	work := t.TempDir()
	tailRepoGit(t, work, "init", "-q")
	tailRepoGit(t, work, "symbolic-ref", "HEAD", "refs/heads/main")
	commit := func(file string) string {
		if err := os.WriteFile(filepath.Join(work, file), []byte("package a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		tailRepoGit(t, work, "add", "-A")
		tailRepoGit(t, work, "commit", "-q", "-m", "c-"+file)
		return tailRepoGit(t, work, "rev-parse", "HEAD")
	}
	binary = commit("a.go")
	tailRepoGit(t, work, "push", "-q", bare, "main")

	checkout := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Dir(checkout), 0o755); err != nil {
		t.Fatal(err)
	}
	tailRepoGit(t, townRoot, "clone", "-q", bare, checkout)

	second = commit("b.go")
	tip = commit("c.go")
	tailRepoGit(t, work, "push", "-q", bare, "main")
	return townRoot, binary, second, tip
}

// TestTailMainPicture_ReadsTheBareReposMainNotTheStaleCheckout: the main picture
// is the bare repo's main, the installed commit is the binary's, and behind is
// counted between them. The checkout beside the bare repo still sits at the
// binary, so reading it would say the binary is current.
func TestTailMainPicture_ReadsTheBareReposMainNotTheStaleCheckout(t *testing.T) {
	t.Parallel()
	townRoot, binary, _, tip := tailRepoRig(t)

	gotTip, installed, behind, err := tailMainPictureOf(townRoot, binary, nil)
	if err != nil {
		t.Fatalf("tailMainPictureOf: %v", err)
	}
	if want := version.ShortCommit(tip); gotTip != want {
		t.Errorf("main tip = %s, want the bare repo's main %s", gotTip, want)
	}
	if want := version.ShortCommit(binary); installed != want {
		t.Errorf("installed = %s, want the binary's commit %s", installed, want)
	}
	if behind != 2 {
		t.Errorf("behind = %d, want 2 commits between the binary and main", behind)
	}
}

// TestTailGitAncestry_AnswersFromTheBareRepo: a commit only the bare repo holds
// is an ancestor of its main, which the stale checkout cannot answer at all, and
// a commit past the tip is not an ancestor of an earlier one.
func TestTailGitAncestry_AnswersFromTheBareRepo(t *testing.T) {
	t.Parallel()
	townRoot, binary, second, tip := tailRepoRig(t)
	ancestor := tailGitAncestry(townRoot)

	if ok, known := ancestor(second, tip); !ok || !known {
		t.Errorf("second is ancestor of tip = (%v, %v), want (true, true) from the bare repo", ok, known)
	}
	if ok, known := ancestor(tip, binary); ok || !known {
		t.Errorf("tip is ancestor of binary = (%v, %v), want (false, true)", ok, known)
	}
}
