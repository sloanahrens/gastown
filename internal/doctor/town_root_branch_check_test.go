package doctor

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runTownRootBranchGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestTownRootBranchCheck_NotAGitRepo(t *testing.T) {
	// A non-git directory means "git branch --show-current" fails — this
	// is a could-not-ask condition, not a verified "on main" state.
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir}

	check := NewTownRootBranchCheck()
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped for non-git dir, got %v: %s", result.Status, result.Message)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
	if len(result.Details) == 0 {
		t.Error("expected Details to carry the underlying error")
	}
}

func TestTownRootBranchCheck_OnMain(t *testing.T) {
	tmpDir := t.TempDir()
	runTownRootBranchGit(t, tmpDir, "init", "-q", "-b", "main")
	runTownRootBranchGit(t, tmpDir, "commit", "-q", "--allow-empty", "-m", "seed")

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewTownRootBranchCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK on main branch, got %v: %s", result.Status, result.Message)
	}
}

func TestTownRootBranchCheck_WrongBranch(t *testing.T) {
	tmpDir := t.TempDir()
	runTownRootBranchGit(t, tmpDir, "init", "-q", "-b", "main")
	runTownRootBranchGit(t, tmpDir, "commit", "-q", "--allow-empty", "-m", "seed")
	runTownRootBranchGit(t, tmpDir, "checkout", "-q", "-b", "some-feature")

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewTownRootBranchCheck()
	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError on wrong branch, got %v: %s", result.Status, result.Message)
	}
}
