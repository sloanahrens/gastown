package refinery

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRootFile returns the path of a file at the repository root. The hook it
// points at lives outside every package, so the test file's own path is the
// only handle on it.
func repoRootFile(t *testing.T, elements ...string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file to find the repository root")
	}
	return filepath.Join(append([]string{filepath.Dir(thisFile), "..", ".."}, elements...)...)
}

// cloneWithRepoHooks clones a fresh bare repo into clonePath and points the
// clone at this repository's .githooks, which is how gt configures every rig
// clone (core.hooksPath=.githooks, internal/git.ConfigureHooksPath).
func cloneWithRepoHooks(t *testing.T, tmpRoot, clonePath string) {
	t.Helper()
	bare := filepath.Join(tmpRoot, "origin.git")
	if _, err := os.Stat(bare); err != nil {
		run(t, tmpRoot, "git", "init", "--bare", "--initial-branch=main", bare)
	}
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, tmpRoot, "git", "clone", bare, clonePath)
	run(t, clonePath, "git", "config", "user.email", "test@test.com")
	run(t, clonePath, "git", "config", "user.name", "Test")
	run(t, clonePath, "git", "config", "core.hooksPath", repoRootFile(t, ".githooks"))
	writeFile(t, clonePath, "README.md", "# Test\n")
	run(t, clonePath, "git", "add", ".")
	run(t, clonePath, "git", "commit", "-m", "initial commit")
}

// commitOutput runs `git commit` in dir and returns its combined output.
func commitOutput(t *testing.T, dir, message string) string {
	t.Helper()
	cmd := exec.Command("git", "commit", "-m", message)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git commit in %s failed: %v\n%s", dir, err, out)
	}
	return string(out)
}

// The pre-commit hook is the advisory half of gt-nnwy: it warns the person
// committing in a rig's refinery worktree, where the merge checkout's tree
// state is load-bearing. It must not fire anywhere else, or every commit in
// the town carries a warning nobody reads.
func TestRefineryPreCommitHook_WarnsOnlyInRefineryWorktrees(t *testing.T) {
	t.Parallel()
	hook := repoRootFile(t, ".githooks", "pre-commit")
	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("expected the hook at %s: %v", hook, err)
	}

	tmpRoot := t.TempDir()
	refinery := filepath.Join(tmpRoot, "gastown", "refinery", "rig")
	polecat := filepath.Join(tmpRoot, "gastown", "polecats", "jade", "gastown")
	cloneWithRepoHooks(t, tmpRoot, refinery)
	cloneWithRepoHooks(t, tmpRoot, polecat)

	writeFile(t, refinery, "scratch.go", "package cmd\n")
	run(t, refinery, "git", "add", "scratch.go")
	if out := commitOutput(t, refinery, "wip: scratch"); !strings.Contains(out, "REFINERY worktree") {
		t.Errorf("committing in the refinery worktree must warn, got output %q", out)
	}

	writeFile(t, polecat, "scratch.go", "package cmd\n")
	run(t, polecat, "git", "add", "scratch.go")
	if out := commitOutput(t, polecat, "feat: scratch"); strings.Contains(out, "REFINERY worktree") {
		t.Errorf("committing in a polecat worktree must not warn, got output %q", out)
	}
}

// The warning tells the committer what to do instead; a warning with no way out
// leaves them committing anyway to finish what they were doing.
func TestRefineryPreCommitHook_NamesWhereToTakeTheWork(t *testing.T) {
	t.Parallel()
	tmpRoot := t.TempDir()
	refinery := filepath.Join(tmpRoot, "gastown", "refinery", "rig")
	cloneWithRepoHooks(t, tmpRoot, refinery)

	writeFile(t, refinery, "scratch.go", "package cmd\n")
	run(t, refinery, "git", "add", "scratch.go")
	out := commitOutput(t, refinery, "wip: scratch")

	for _, want := range []string{"not a working copy", "gt crew", "unused/refinery-rig-"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning must mention %q, got output %q", want, out)
		}
	}
}
