//go:build integration

package cmd

import (
	"fmt"
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

// TestIntegrationTailMainPicture_ReadsTheBareReposMainNotTheStaleCheckout: the main
// picture is the bare repo's main, the installed commit is the binary's, and
// behind is counted between them. The checkout beside the bare repo still sits
// at the binary, so reading it would say the binary is current.
func TestIntegrationTailMainPicture_ReadsTheBareReposMainNotTheStaleCheckout(t *testing.T) {
	t.Parallel()
	townRoot, binary, _, tip := tailRepoRig(t)

	gotTip, installed, behind, err := tailMainPictureIn(townRoot, binary, nil, os.Stat, version.CheckStaleBinaryOf)
	if err != nil {
		t.Fatalf("tailMainPictureIn: %v", err)
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

// TestIntegrationTailMainPicture_WithoutABareRepoReadsTheCheckoutAndSaysSo: a
// town with no .repo.git reads the gt source checkout at mayor/rig, the layout a
// real town has, and the why-log names the checkout that answered.
func TestIntegrationTailMainPicture_WithoutABareRepoReadsTheCheckoutAndSaysSo(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	checkout := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(checkout, "cmd", "gt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "cmd", "gt", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tailRepoGit(t, checkout, "init", "-q")
	tailRepoGit(t, checkout, "symbolic-ref", "HEAD", "refs/heads/main")
	tailRepoGit(t, checkout, "add", "-A")
	tailRepoGit(t, checkout, "commit", "-q", "-m", "c-main")
	binary := tailRepoGit(t, checkout, "rev-parse", "HEAD")

	var lines []string
	why := newDashWhyLog(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	gotTip, installed, behind, err := tailMainPictureIn(townRoot, binary, why, os.Stat, version.CheckStaleBinaryOf)
	if err != nil {
		t.Fatalf("tailMainPictureIn: %v", err)
	}
	if want := version.ShortCommit(binary); gotTip != want || installed != want || behind != 0 {
		t.Errorf("picture = (%s, %s, %d), want the checkout's own commit %s, current", gotTip, installed, behind, want)
	}
	want := "deploy repo: " + filepath.Join(townRoot, "gastown", ".repo.git") + " is absent; reading the source checkout " + checkout
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("why-log = %q, want exactly %q", lines, want)
	}
}

// TestIntegrationTailGitAncestry_AnswersFromTheBareRepo: a commit only the bare
// repo holds is an ancestor of its main, which the stale checkout cannot answer
// at all, and a commit past the tip is not an ancestor of an earlier one.
func TestIntegrationTailGitAncestry_AnswersFromTheBareRepo(t *testing.T) {
	t.Parallel()
	townRoot, binary, second, tip := tailRepoRig(t)
	ancestor := tailGitAncestry(townRoot, nil)

	if ok, known := ancestor(second, tip); !ok || !known {
		t.Errorf("second is ancestor of tip = (%v, %v), want (true, true) from the bare repo", ok, known)
	}
	if ok, known := ancestor(tip, binary); ok || !known {
		t.Errorf("tip is ancestor of binary = (%v, %v), want (false, true)", ok, known)
	}
}
