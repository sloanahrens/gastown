//go:build integration

package gtwrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir for a fixture and returns its output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestIntegrationOSGitReadsARealRepositoryAndLinkedWorktree: OSGit reads the
// branch and HEAD of a repository and of a linked worktree (a .git file that
// points at a gitdir), answers "not a worktree" for a plain directory, and is
// not steered by GIT_DIR, GIT_WORK_TREE or GIT_INDEX_FILE in the environment.
func TestIntegrationOSGitReadsARealRepositoryAndLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gitIn(t, root, "init", "-b", "main", repo)
	gitIn(t, repo, "commit", "--allow-empty", "-m", "first")
	mainHead := gitIn(t, repo, "rev-parse", "HEAD")
	linked := filepath.Join(root, "linked")
	gitIn(t, repo, "worktree", "add", "-b", "crew/a/gt-x", linked)
	gitIn(t, linked, "commit", "--allow-empty", "-m", "second")
	linkedHead := gitIn(t, linked, "rev-parse", "HEAD")
	if info, err := os.Stat(filepath.Join(linked, ".git")); err != nil || info.IsDir() {
		t.Fatalf("the linked worktree's .git = %v, %v; want a file pointing at a gitdir", info, err)
	}

	// An inherited override must not move the read to another repository.
	t.Setenv("GIT_DIR", filepath.Join(repo, ".git"))
	t.Setenv("GIT_WORK_TREE", repo)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(repo, ".git", "index"))

	for _, tc := range []struct {
		name string
		dir  string
		want WorktreeFacts
	}{
		{"repository", repo, WorktreeFacts{Branch: "main", Head: mainHead}},
		{"linked worktree", linked, WorktreeFacts{Branch: "crew/a/gt-x", Head: linkedHead}},
	} {
		got, ok, err := ReadWorktreeFacts(OSGit, tc.dir)
		if err != nil || !ok || got != tc.want {
			t.Errorf("%s: ReadWorktreeFacts = %+v, ok %t, err %v; want %+v", tc.name, got, ok, err, tc.want)
		}
	}

	plain := t.TempDir()
	got, ok, err := ReadWorktreeFacts(OSGit, plain)
	if err != nil || ok || got != (WorktreeFacts{}) {
		t.Errorf("plain directory: ReadWorktreeFacts = %+v, ok %t, err %v; want nothing and no error", got, ok, err)
	}
}
