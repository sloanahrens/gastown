package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// The scenarios below reproduce gt-ozo4 against real repositories: a polecat
// branch whose machine-generated checkpoint commit added a throwaway file.
// gt done collapses the branch into one commit carrying HEAD's tree, so the
// file is submitted even after the polecat deletes it from the working tree —
// the same fold the checkpoint-dog and gt-pvx fixes prevent at their ends.

// newThrowawayScenario builds origin.git, a seed checkout standing in for main,
// and a polecat checkout on its own branch whose tip is a "WIP: checkpoint
// (auto)" commit adding scratch.go's namesake. The caller adds real work or
// cleans the branch up from there.
func newThrowawayScenario(t *testing.T, throwawayPath string) string {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")

	runGitCmd(t, "", "init", "--bare", remote)
	// Point the bare repo's HEAD at main explicitly: git init's default branch
	// name is host-configurable, and the clones below check out whatever HEAD
	// names.
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", remote, seed)
	runGitCmd(t, seed, "config", "user.email", "seed@example.com")
	runGitCmd(t, seed, "config", "user.name", "Seed")
	writeTestFile(t, filepath.Join(seed, "README.md"), "# base\n")
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "push", "origin", "main")

	runGitCmd(t, "", "clone", remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "switch", "-c", "polecat/garnet/gt-ozo4")

	writeTestFile(t, filepath.Join(polecat, "real.go"), "package main\n")
	runGitCmd(t, polecat, "add", "-A")
	runGitCmd(t, polecat, "commit", "-m", "real work (gt-ozo4)")

	if err := os.MkdirAll(filepath.Dir(filepath.Join(polecat, throwawayPath)), 0o755); err != nil {
		t.Fatalf("create throwaway dir: %v", err)
	}
	writeTestFile(t, filepath.Join(polecat, throwawayPath), "package main\n")
	runGitCmd(t, polecat, "add", "-A")
	runGitCmd(t, polecat, "commit", "-m", "WIP: checkpoint (auto)")

	// The polecat cleans up the way it naturally would: delete the file.
	if err := os.Remove(filepath.Join(polecat, throwawayPath)); err != nil {
		t.Fatalf("remove throwaway file: %v", err)
	}

	return polecat
}

func TestReportThrowawayPaths_RefusesBranchAddingScratchFile(t *testing.T) {
	polecat := newThrowawayScenario(t, "internal/util/zz_livecheck_test.go")

	err := reportThrowawayPaths(git.NewGit(polecat), "origin/main")
	if err == nil {
		t.Fatal("reportThrowawayPaths accepted a branch that adds a throwaway file")
	}
	msg := err.Error()
	if !strings.Contains(msg, "internal/util/zz_livecheck_test.go") {
		t.Errorf("refusal does not name the offending path: %s", msg)
	}
	if !strings.Contains(msg, "git rm --cached") {
		t.Errorf("refusal does not say how to take the file off the branch: %s", msg)
	}
	if strings.Contains(msg, "allow-throwaway-paths") {
		t.Errorf("refusal names the flag that overrides it; agents self-bypass: %s", msg)
	}
}

// TestReportThrowawayPaths_LeavesTheBranchUntouched pins why the gate sits
// before the commit-message squash: the polecat has to find the branch exactly
// as it left it, so the fix it runs by hand is applied to the history it was
// shown.
func TestReportThrowawayPaths_LeavesTheBranchUntouched(t *testing.T) {
	polecat := newThrowawayScenario(t, "internal/util/zz_livecheck_test.go")
	g := git.NewGit(polecat)
	before, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}

	if err := reportThrowawayPaths(g, "origin/main"); err == nil {
		t.Fatal("reportThrowawayPaths accepted a branch that adds a throwaway file")
	}

	after, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD after the refusal: %v", err)
	}
	if after != before {
		t.Fatalf("refusal rewrote the branch: HEAD moved from %s to %s", before, after)
	}
}

// TestReportThrowawayPaths_QuotesPathInRemediation holds the prescribed command
// to being runnable: an unquoted path with a space in it would split into two
// arguments, so the polecat would follow the refusal and still be stuck.
func TestReportThrowawayPaths_QuotesPathInRemediation(t *testing.T) {
	polecat := newThrowawayScenario(t, "scratch/notes copy.md")

	err := reportThrowawayPaths(git.NewGit(polecat), "origin/main")
	if err == nil {
		t.Fatal("reportThrowawayPaths accepted a branch that adds a throwaway file")
	}
	if !strings.Contains(err.Error(), "'scratch/notes copy.md'") {
		t.Errorf("remediation leaves the path unquoted and unrunnable: %s", err)
	}
}

// TestReportThrowawayPaths_AllowsRealWork is the false-positive control: an
// ordinary branch, including one carrying a WIP checkpoint commit, must pass.
func TestReportThrowawayPaths_AllowsRealWork(t *testing.T) {
	polecat := newThrowawayScenario(t, "internal/util/zz_livecheck_test.go")
	runGitCmd(t, polecat, "rm", "--cached", "--", "internal/util/zz_livecheck_test.go")
	runGitCmd(t, polecat, "commit", "-m", "remove throwaway files")

	if err := reportThrowawayPaths(git.NewGit(polecat), "origin/main"); err != nil {
		t.Fatalf("reportThrowawayPaths refused a clean branch: %v", err)
	}
}

// TestReportThrowawayPaths_UnresolvableBaseFailsClosed pins the fail-closed
// contract: a check that could not run must not read as a check that passed.
func TestReportThrowawayPaths_UnresolvableBaseFailsClosed(t *testing.T) {
	polecat := newThrowawayScenario(t, "internal/util/zz_livecheck_test.go")

	err := reportThrowawayPaths(git.NewGit(polecat), "origin/does-not-exist")
	if err == nil {
		t.Fatal("reportThrowawayPaths returned nil for an unresolvable base, want a refusal")
	}
	if !strings.Contains(err.Error(), "cannot check branch for throwaway files") {
		t.Errorf("failure does not report that the check could not run: %v", err)
	}
}
