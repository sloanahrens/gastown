//go:build integration

// Integration tier: whether git reads a worktree as clean is git's call, so
// this test runs real git. gitfake seeds every index from HEAD, so it cannot
// show git's reading of a missing index as empty.

package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Fix re-registers a worktree whose registration directory is gone, the way a
// dog worktree looks after an rsync. The manual registration writes no index,
// and git reads a missing index as empty, so every tracked file showed as a
// staged deletion (gt-n9ps4). After Fix the worktree must read clean, and an
// edit the user has not committed must still show as a modification.
func TestIntegrationWorktreeGitdirFixPopulatesIndex(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(`{"repo":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// A real bare repo at the path the rig uses, with tracked files on main.
	seed := filepath.Join(tmpDir, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, seed, "init", "--initial-branch=main")
	runRealGit(t, seed, "add", ".")
	runRealGit(t, seed, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "init")

	bareRepo := filepath.Join(rigDir, ".repo.git")
	runRealGit(t, tmpDir, "clone", "--bare", seed, bareRepo)

	wtPath := filepath.Join(rigDir, "polecats", "nux", rigName)
	runRealGit(t, bareRepo, "worktree", "add", wtPath, "main")

	// Losing the registration directory leaves the checkout behind with a
	// .git file that points nowhere.
	if err := os.RemoveAll(filepath.Join(bareRepo, "worktrees", rigName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "README"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := NewWorktreeGitdirCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError for a worktree with no registration, got %v: %s", result.Status, result.Message)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Exactly the one edit: no staged deletions, no untracked files.
	out, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status after Fix: %v\n%s", err, out)
	}
	if got, want := string(out), " M README\n"; got != want {
		t.Fatalf("git status after Fix:\n%s\nwant:\n%s", got, want)
	}
}
