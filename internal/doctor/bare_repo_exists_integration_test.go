//go:build integration

// Integration tier: deciding whether a worktree is usable is git's call, so
// these tests run real git. internal/doctor's unit tier runs none
// (internal/testpolicy/gitfree.txt, docs/testing.md "Seams for external
// tools"), which is why this file carries the integration tag.

package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runRealGit runs a git command and fails the test if it does not succeed.
func runRealGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// A worktree whose registration directory was removed is dead even though the
// worktree directory still exists: git reports "not a git repository". Fix has
// to leave it usable, which takes commondir as well as gitdir and HEAD
// (gt-ux2hv). Real git, because only git can say whether the entry it reads is
// one it can use.
//
// This is the gt-ux2hv acceptance scenario it names
// TestBareRepoExistsFixReregistersUsableWorktree in
// internal/doctor/rig_check_test.go; the integration tag and TestIntegration
// name are the unit tier's policy, as the file header says (gt-9d3zt).
func TestIntegrationBareRepoExistsFixReregistersUsableWorktree(t *testing.T) {
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

	// A real bare repo at the path the rig uses, with a commit on main.
	seed := filepath.Join(tmpDir, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, seed, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, seed, "add", "README")
	runRealGit(t, seed, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "init")

	bareRepo := filepath.Join(rigDir, ".repo.git")
	runRealGit(t, tmpDir, "clone", "--bare", seed, bareRepo)

	// A polecat worktree on its own branch, registered by git.
	wtPath := filepath.Join(rigDir, "polecats", "nux", rigName)
	runRealGit(t, bareRepo, "worktree", "add", wtPath, "-b", "polecat/nux/work")

	// Losing the registration directory is how the bug showed up.
	if err := os.RemoveAll(filepath.Join(bareRepo, "worktrees", rigName)); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", wtPath, "status").Run(); err == nil {
		t.Fatal("worktree is still usable after its registration was removed; this no longer reproduces the bug")
	}

	check := NewBareRepoExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError for a worktree with no registration, got %v: %s", result.Status, result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// The verdict Run gives after Fix has to be the truth, so re-run it: a
	// repaired worktree must report healthy, and git must agree.
	if result := NewBareRepoExistsCheck().Run(ctx); result.Status != StatusOK {
		t.Fatalf("expected StatusOK after Fix, got %v: %s", result.Status, result.Message)
	}
	if err := exec.Command("git", "-C", wtPath, "status").Run(); err != nil {
		t.Fatalf("worktree still unusable after Fix (missing commondir?): %v", err)
	}
}
