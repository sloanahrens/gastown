//go:build integration

package git_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// realEnv builds gitfake's contract fixtures with git commands and opens
// *git.Git, so the contract pins the fake to real git. Commits are made with plumbing, so a bare repository needs no
// checkout, and carry increasing dates, so git's log order is the order they
// were made, as in the fake.
type realEnv struct {
	commits int
}

func (e *realEnv) git(t testing.TB, dir, stdin string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

func (e *realEnv) InitBare(t testing.TB, dir string) {
	t.Helper()
	e.git(t, filepath.Dir(dir), "", nil, "init", "-q", "--bare", "-b", "main", dir)
}

func (e *realEnv) Commit(t testing.TB, dir, branch, message string, files map[string]string) string {
	t.Helper()
	ref := "refs/heads/" + branch
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	var parent string
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", ref)
	if out, err := cmd.Output(); err == nil {
		parent = strings.TrimSpace(string(out))
		e.git(t, dir, "", env, "read-tree", parent)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		blob := e.git(t, dir, files[p], nil, "hash-object", "-w", "--stdin")
		e.git(t, dir, "", env, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+p)
	}
	tree := e.git(t, dir, "", env, "write-tree")
	e.commits++
	date := fmt.Sprintf("@%d +0000", 1_700_000_000+e.commits)
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	id := e.git(t, dir, "", []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, args...)
	e.git(t, dir, "", nil, "update-ref", ref, id)
	return id
}

func (e *realEnv) SetRef(t testing.TB, dir, ref, id string) {
	t.Helper()
	e.git(t, dir, "", nil, "update-ref", ref, id)
}

func (e *realEnv) Clone(t testing.TB, src, dest string) {
	t.Helper()
	e.git(t, filepath.Dir(dest), "", nil, "clone", "-q", src, dest)
}

func (e *realEnv) Open(dir string) gitfake.Repo { return git.NewGit(dir) }

func TestIntegrationGitfakeRepoContract(t *testing.T) {
	gitfake.RunRepoContract(t, func(t *testing.T) gitfake.Env { return &realEnv{} })
}

// CommitWorktree is git add -A; git commit in the checkout at dir.
func (e *realEnv) CommitWorktree(t testing.TB, dir, message string) string {
	t.Helper()
	e.git(t, dir, "", nil, "add", "-A")
	e.commits++
	date := fmt.Sprintf("@%d +0000", 1_700_000_000+e.commits)
	e.git(t, dir, "", []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "commit", "-q", "-m", message)
	return e.git(t, dir, "", nil, "rev-parse", "HEAD")
}

func (e *realEnv) Stash(t testing.TB, dir, message string) {
	t.Helper()
	e.git(t, dir, "", nil, "stash", "push", "-q", "-m", message)
}

func (e *realEnv) OpenWorktreeRepo(dir string) gitfake.WorktreeRepo { return git.NewGit(dir) }

func (e *realEnv) OpenDir(gitDir, workDir string) gitfake.WorktreeRepo {
	return git.NewGitWithDir(gitDir, workDir)
}

func TestIntegrationGitfakeWorktreeContract(t *testing.T) {
	gitfake.RunWorktreeContract(t, func(t *testing.T) gitfake.WorktreeEnv { return &realEnv{} })
}
