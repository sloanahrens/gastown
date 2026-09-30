package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestParseWorktreeConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		output   string
		wantPath string
	}{
		{
			name:     "older git: already checked out at",
			output:   "fatal: 'main' is already checked out at '/home/user/gt/rig/.repo.git'",
			wantPath: "/home/user/gt/rig/.repo.git",
		},
		{
			name:     "newer git: already used by worktree at",
			output:   "fatal: 'main' is already used by worktree at '/home/user/gt/rig/.repo.git'",
			wantPath: "/home/user/gt/rig/.repo.git",
		},
		{
			name:     "with trailing newline",
			output:   "fatal: 'main' is already checked out at '/tmp/bare.git'\n",
			wantPath: "/tmp/bare.git",
		},
		{
			name:     "different branch name",
			output:   "fatal: 'develop' is already checked out at '/some/path/worktree'",
			wantPath: "/some/path/worktree",
		},
		{
			name:     "not a worktree conflict",
			output:   "error: pathspec 'main' did not match any file(s) known to git",
			wantPath: "",
		},
		{
			name:     "empty output",
			output:   "",
			wantPath: "",
		},
		{
			name:     "partial match no closing quote",
			output:   "fatal: 'main' is already checked out at '/broken",
			wantPath: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWorktreeConflict(tt.output)
			if got != tt.wantPath {
				t.Errorf("parseWorktreeConflict(%q) = %q, want %q", tt.output, got, tt.wantPath)
			}
		})
	}
}

// A branch a bare repository's HEAD names (.repo.git on main) is free for a
// worktree to check out: git does not count a bare HEAD as checked out, so
// the switch succeeds without the detach-and-retry.
func TestCheckoutWithWorktreeRetry_BareRepoConflict(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	tmpDir := t.TempDir()
	bareRepo := fakeRemote(t, f, filepath.Join(tmpDir, "rig", ".repo.git"), nil)
	worktreeDir := filepath.Join(tmpDir, "rig", "refinery", "rig")
	if err := f.OpenBranchRepo(bareRepo).WorktreeAddFromRef(worktreeDir, "integration/test", "main"); err != nil {
		t.Fatal(err)
	}

	ctx := withGit(&CheckContext{}, f)
	if err := NewBranchCheck().checkoutWithWorktreeRetry(ctx, worktreeDir, "main"); err != nil {
		t.Fatalf("checkoutWithWorktreeRetry should succeed, got: %v", err)
	}
	if branch, _ := f.OpenBranchRepo(worktreeDir).CurrentBranch(); branch != "main" {
		t.Errorf("expected worktree to be on 'main', got %q", branch)
	}
}

// TestCheckoutWithWorktreeRetry_NonBareRepoConflict verifies that conflicts
// with non-bare repos produce a clear error instead of silently failing.
func TestCheckoutWithWorktreeRetry_NonBareRepoConflict(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	tmpDir := t.TempDir()
	mainRepo := filepath.Join(tmpDir, "main-clone")
	fakeClone(t, f, mainRepo)
	worktreeDir := filepath.Join(tmpDir, "worktree")
	if err := f.OpenBranchRepo(mainRepo).WorktreeAddFromRef(worktreeDir, "feature", "main"); err != nil {
		t.Fatal(err)
	}

	err := NewBranchCheck().checkoutWithWorktreeRetry(withGit(&CheckContext{}, f), worktreeDir, "main")
	if err == nil {
		t.Fatal("expected error for non-bare repo conflict, got nil")
	}
	if !strings.Contains(err.Error(), "not a bare repo") || !strings.Contains(err.Error(), mainRepo) {
		t.Errorf("expected error to mention 'not a bare repo' and the holder, got: %v", err)
	}
}

// TestCheckoutWithWorktreeRetry_NormalCheckout verifies normal checkout
// (no worktree conflict) still works.
func TestCheckoutWithWorktreeRetry_NormalCheckout(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	repo := filepath.Join(t.TempDir(), "repo")
	fakeClone(t, f, repo)
	if err := f.OpenBranchRepo(repo).CheckoutNewBranch("feature", "HEAD"); err != nil {
		t.Fatal(err)
	}

	if err := NewBranchCheck().checkoutWithWorktreeRetry(withGit(&CheckContext{}, f), repo, "main"); err != nil {
		t.Fatalf("expected normal checkout to succeed, got: %v", err)
	}
	if branch, _ := f.OpenBranchRepo(repo).CurrentBranch(); branch != "main" {
		t.Errorf("expected repo to be on 'main', got %q", branch)
	}
}

// TestCheckoutWithWorktreeRetry_BranchNotFound verifies clear error for missing branch.
func TestCheckoutWithWorktreeRetry_BranchNotFound(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	repo := filepath.Join(t.TempDir(), "repo")
	fakeClone(t, f, repo)

	err := NewBranchCheck().checkoutWithWorktreeRetry(withGit(&CheckContext{}, f), repo, "nonexistent-branch")
	if err == nil {
		t.Fatal("expected error for nonexistent branch, got nil")
	}
	if !strings.Contains(err.Error(), "git checkout nonexistent-branch failed") || !strings.Contains(err.Error(), "did not match") {
		t.Errorf("expected error about failed checkout carrying git's message, got: %v", err)
	}
}

// runGit is a test helper that runs git commands and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s) failed: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}
